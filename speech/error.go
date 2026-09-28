package speech

import (
	"fmt"
	"time"
)

// ErrorCategory identifies which part of a batch speech call failed.
type ErrorCategory string

const (
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	ErrorUnsupported    ErrorCategory = "unsupported"
	ErrorAuthentication ErrorCategory = "authentication"
	// ErrorRateLimited covers HTTP 429, including quota exhaustion. Inspect
	// Code and Retryable to distinguish an account limit from throttling.
	ErrorRateLimited ErrorCategory = "rate_limited"
	ErrorProvider    ErrorCategory = "provider"
	ErrorCanceled    ErrorCategory = "canceled"
	ErrorTransport   ErrorCategory = "transport"
	ErrorProtocol    ErrorCategory = "protocol"
)

// Error keeps a failure classification separate from retry authorization.
// RemoteOutcomeUnknown means the provider may have processed the call despite
// the local failure; callers must reconcile before attempting a replay.
// Provider response text is deliberately omitted from Error and Error().
type Error struct {
	Category ErrorCategory
	Status   int
	// Code is a bounded, machine-readable provider error code when supplied.
	Code string
	// RetryAfter is a valid, positive provider Retry-After hint, or zero.
	RetryAfter time.Duration
	// Retryable is an advisory classification of an explicit HTTP rejection,
	// not permission to replay a call with an uncertain remote outcome.
	Retryable            bool
	RemoteOutcomeUnknown bool
	Cause                error
}

func (e *Error) Error() string {
	if e == nil {
		return "speech: nil error"
	}
	if e.Cause != nil {
		return fmt.Sprintf("speech: %s: %v", e.Category, e.Cause)
	}
	if e.Status != 0 {
		return fmt.Sprintf("speech: %s (HTTP %d)", e.Category, e.Status)
	}
	return "speech: " + string(e.Category)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
