// Ported from Dive (65c393f), Apache-2.0.

package providerhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProviderError represents an error returned by an LLM provider API.
type ProviderError struct {
	statusCode int
	body       string
	code       string
	retryAfter time.Duration
	hasRetry   bool
}

// NewError preserves provider failure details without executing retry policy.
func NewError(
	statusCode int,
	body string,
	opts ...ErrorOption,
) *ProviderError {
	providerErr := &ProviderError{statusCode: statusCode, body: body}
	for _, opt := range opts {
		opt(providerErr)
	}
	if providerErr.code == "" {
		providerErr.code = errorCodeFromBody(body)
	}
	return providerErr
}

// Error retains the status and body for diagnosis without inferring retry policy.
func (e *ProviderError) Error() string {
	return fmt.Sprintf(
		"providerhttp: api error (status %d): %s",
		e.statusCode,
		e.body,
	)
}

// StatusCode returns the HTTP rejection status preserved from the response.
func (e *ProviderError) StatusCode() int {
	return e.statusCode
}

// Code returns the provider's machine-readable error code, e.g. "slow_down" or
// "server_is_overloaded". It is empty when the provider did not supply one.
func (e *ProviderError) Code() string {
	return e.code
}

// RetryAfter returns the delay the provider asked for in its Retry-After
// header, and whether it sent one.
func (e *ProviderError) RetryAfter() (time.Duration, bool) {
	return e.retryAfter, e.hasRetry
}

// ErrorOption supplies additional detail about a failed provider response.
type ErrorOption func(*ProviderError)

// WithErrorCode sets the provider's machine-readable error code. Use it when
// the code has already been parsed — an SDK error value, say — since the body
// passed to NewError is then a message rather than the raw JSON envelope.
func WithErrorCode(code string) ErrorOption {
	return func(e *ProviderError) {
		e.code = code
	}
}

// WithErrorHeader supplies the failed response's headers, so a Retry-After
// hint is carried on the error. A nil header is ignored.
func WithErrorHeader(header http.Header) ErrorOption {
	return func(e *ProviderError) {
		if after, ok := parseRetryAfter(header, time.Now()); ok {
			e.retryAfter, e.hasRetry = after, true
		}
	}
}

// RetryAfter reports the delay a provider asked for on err, if any.
func RetryAfter(err error) (time.Duration, bool) {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return 0, false
	}
	return providerErr.RetryAfter()
}

// terminalErrorCodes are provider error codes that never succeed on retry even
// though their status code normally would. OpenAI returns all of these with
// 429, the same status as an ordinary rate limit, so the status code alone
// cannot tell a request that is merely too fast from an account that is out of
// money. Retrying a spend limit burns the attempt budget and delays the error
// the caller needs to see.
var terminalErrorCodes = map[string]bool{
	"credit_balance_exhausted":          true,
	"insufficient_quota":                true,
	"organization_spend_limit_exceeded": true,
	"organization_usage_limit_exceeded": true,
	"project_spend_limit_exceeded":      true,
}

// shouldRetry supplies an advisory retry hint for an explicit HTTP failure.
func shouldRetry(statusCode int, code string) bool {
	if terminalErrorCodes[code] {
		return false
	}
	return statusCode == http.StatusTooManyRequests || // 429
		statusCode == http.StatusInternalServerError || // 500
		statusCode == http.StatusBadGateway || // 502
		statusCode == http.StatusServiceUnavailable || // 503
		statusCode == http.StatusGatewayTimeout || // 504
		statusCode == 520 || // Cloudflare
		statusCode == 529 // Anthropic overloaded_error
}

// errorCodeFromBody pulls the error code out of a JSON error body. It accepts
// both the nested shape OpenAI and Anthropic use, {"error":{"code":...}}, and
// the flat {"code":...} some OpenAI-compatible providers return. A body that
// is a plain message rather than JSON yields an empty code.
func errorCodeFromBody(body string) string {
	// The members are decoded one at a time rather than into a single struct,
	// because a provider that sends "error" as a bare string would fail the
	// whole unmarshal and lose a top-level "code" sitting beside it.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return ""
	}
	var nested struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(
		envelope["error"],
		&nested,
	); err == nil && nested.Code != "" {
		return nested.Code
	}
	var code string
	if err := json.Unmarshal(envelope["code"], &code); err == nil {
		return code
	}
	return ""
}

// parseRetryAfter interprets a Retry-After header in both forms RFC 9110
// allows: a delay in seconds, or the HTTP-date at which to retry. Fractional
// seconds are accepted because providers send them even though the grammar
// calls for an integer.
func parseRetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(
			seconds,
			0,
		) {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if wait := deadline.Sub(now); wait > 0 {
			return wait, true
		}
		return 0, false
	}
	return 0, false
}
