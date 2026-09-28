// Ported from Dive (65c393f), Apache-2.0.

package providerhttp

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestShouldRetryClassifiesHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "bad request", status: 400},
		{name: "unauthorized", status: 401},
		{name: "forbidden", status: 403},
		{name: "not found", status: 404},
		{name: "rate limit", status: 429, want: true},
		{name: "internal error", status: 500, want: true},
		{name: "bad gateway", status: 502, want: true},
		{name: "unavailable", status: 503, want: true},
		{name: "gateway timeout", status: 504, want: true},
		{name: "cloudflare error", status: 520, want: true},
		{name: "anthropic overloaded", status: 529, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRetry(
				tt.status,
				"",
			); got != tt.want {
				t.Fatalf(
					"retry advice for HTTP %d = %t, want %t",
					tt.status,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestShouldRetryUsesProviderCode(t *testing.T) {
	// Quota exhaustion and a temporary rate limit can share the same status.
	tests := []struct {
		name   string
		status int
		code   string
		want   bool
	}{
		{
			name:   "quota exhausted",
			status: 429,
			code:   "insufficient_quota",
		},
		{
			name:   "balance exhausted",
			status: 429,
			code:   "credit_balance_exhausted",
		},
		{
			name:   "organization spend limit",
			status: 429,
			code:   "organization_spend_limit_exceeded",
		},
		{
			name:   "project spend limit",
			status: 429,
			code:   "project_spend_limit_exceeded",
		},
		{
			name:   "organization usage limit",
			status: 429,
			code:   "organization_usage_limit_exceeded",
		},
		{
			name:   "rate limit",
			status: 429,
			code:   "slow_down",
			want:   true,
		},
		{
			name:   "overloaded",
			status: 503,
			code:   "server_is_overloaded",
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRetry(
				tt.status,
				tt.code,
			); got != tt.want {
				t.Fatalf(
					"retry advice for %s = %t, want %t",
					tt.code,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestProviderErrorReadsCodeFromBody(t *testing.T) {
	tests := []struct {
		name, body, code string
		retry            bool
	}{
		{
			name: "nested quota",
			body: `{"error":{"code":"insufficient_quota","message":"no credits"}}`,
			code: "insufficient_quota",
		},
		{
			name: "flat quota",
			body: `{"code":"insufficient_quota","error":"no credits"}`,
			code: "insufficient_quota",
		},
		{
			name:  "temporary limit",
			body:  `{"error":{"code":"slow_down"}}`,
			code:  "slow_down",
			retry: true,
		},
		{name: "plain body", body: "not json at all", retry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewError(429, tt.body)
			if err.Code() != tt.code || shouldRetry(
				err.StatusCode(),
				err.Code(),
			) != tt.retry {
				t.Fatalf(
					"body lost its code or retry meaning: %+v",
					err,
				)
			}
		})
	}
}

func TestProviderErrorExplicitCodeWins(t *testing.T) {
	err := NewError(
		429,
		"You exceeded your current quota",
		WithErrorCode("insufficient_quota"),
	)
	providerErr, ok := errors.AsType[*ProviderError](err)
	if !ok {
		t.Fatal("provider error identity was lost")
	}
	if providerErr.Code() != "insufficient_quota" || shouldRetry(
		providerErr.StatusCode(),
		providerErr.Code(),
	) {
		t.Fatal("explicit quota code did not prevent retry advice")
	}
}

func TestProviderErrorCarriesRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		want     time.Duration
		reported bool
	}{
		{
			name: "temporary limit",
			err: NewError(
				429,
				"slow down",
				WithErrorHeader(http.Header{
					"Retry-After": {
						"7",
					},
				}),
			),
			want:     7 * time.Second,
			reported: true,
		},
		{
			name: "terminal quota",
			err: NewError(
				429,
				"no credits",
				WithErrorCode("insufficient_quota"),
				WithErrorHeader(http.Header{
					"Retry-After": {
						"7",
					},
				}),
			),
			want:     7 * time.Second,
			reported: true,
		},
		{name: "missing hint", err: NewError(429, "slow down")},
		{
			name: "other error",
			err:  errors.New("test: not a provider error"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			after, reported := RetryAfter(tt.err)
			if after != tt.want ||
				reported != tt.reported {
				t.Fatalf(
					"retry hint = %v, %t; want %v, %t",
					after,
					reported,
					tt.want,
					tt.reported,
				)
			}
		})
	}
}

func TestParseRetryAfterAcceptsHeaderFormats(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, value string
		want        time.Duration
		ok          bool
	}{
		{
			name:  "seconds",
			value: "30",
			want:  30 * time.Second,
			ok:    true,
		},
		{
			name:  "fractional seconds",
			value: "1.5",
			want:  1500 * time.Millisecond,
			ok:    true,
		},
		{
			name:  "padded",
			value: " 30 ",
			want:  30 * time.Second,
			ok:    true,
		},
		{
			name:  "http date",
			value: "Thu, 03 Sep 2026 12:00:20 GMT",
			want:  20 * time.Second,
			ok:    true,
		},
		{
			name:  "http date in the past",
			value: "Thu, 03 Sep 2026 11:59:00 GMT",
		},
		{name: "zero", value: "0"},
		{name: "negative", value: "-5"},
		{name: "not a number", value: "soon"},
		{name: "infinity", value: "Inf"},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.value != "" {
				header.Set("Retry-After", tt.value)
			}
			got, ok := parseRetryAfter(header, now)
			if got != tt.want || ok != tt.ok {
				t.Fatalf(
					"parsed retry hint = %v, %t; want %v, %t",
					got,
					ok,
					tt.want,
					tt.ok,
				)
			}
		})
	}
}

func TestParseRetryAfterAcceptsNilHeaders(t *testing.T) {
	if _, ok := parseRetryAfter(nil, time.Now()); ok {
		t.Fatal("nil headers reported a retry hint")
	}
}
