package media

import (
	"fmt"
	"slices"
	"time"
)

// ErrorCategory classifies a media operation failure without deciding whether
// another remote attempt is safe.
type ErrorCategory string

const (
	// ErrorInvalidRequest means the caller supplied an invalid value.
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	// ErrorUnsupported means the selected API cannot encode the operation.
	ErrorUnsupported ErrorCategory = "unsupported"
	// ErrorAuthentication means the destination rejected its credentials.
	ErrorAuthentication ErrorCategory = "authentication"
	// ErrorRateLimited means the destination asked the caller to slow down.
	ErrorRateLimited ErrorCategory = "rate_limited"
	// ErrorOverloaded means the destination could not serve the request.
	ErrorOverloaded ErrorCategory = "overloaded"
	// ErrorTransport means dispatch or response reading failed.
	ErrorTransport ErrorCategory = "transport"
	// ErrorProtocol means a successful response could not be interpreted.
	ErrorProtocol ErrorCategory = "protocol"
)

// Error keeps provider failure details without hiding a submission whose
// result is unknown.
// Retryable is an advisory classification, not permission to resubmit a job.
type Error struct {
	Provider   string
	Category   ErrorCategory
	Status     int
	Code       string
	Body       []byte
	RetryAfter time.Duration
	Retryable  bool
	Cause      error
}

// Error gives a concise description without echoing the provider response body.
func (e *Error) Error() string {
	if e == nil {
		return "media: nil error"
	}
	if e.Cause != nil {
		return fmt.Sprintf(
			"media: %s %s: %v",
			e.Provider,
			e.Category,
			e.Cause,
		)
	}
	if e.Code != "" {
		return fmt.Sprintf(
			"media: %s %s: %s",
			e.Provider,
			e.Category,
			e.Code,
		)
	}
	return fmt.Sprintf(
		"media: %s %s (HTTP %d)",
		e.Provider,
		e.Category,
		e.Status,
	)
}

// Unwrap exposes the underlying transport or provider error when present.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Clone detaches the raw response body from the original error.
func (e *Error) Clone() *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Body = slices.Clone(e.Body)
	return &clone
}
