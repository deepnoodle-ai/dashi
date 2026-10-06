package selective

import (
	"errors"

	"github.com/deepnoodle-ai/dashi/decide/internal/cause"
	"github.com/deepnoodle-ai/decide/patterns/compact"
)

// Cause names, in a few words, why Start returned Base's Start instead of
// its own. A host shows it without reading Selection.Fallback's text.
type Cause string

// Causes of a Decide failure. They use the same words as toolcheck.Cause.
const (
	CauseTimeout       Cause = cause.Timeout
	CauseCanceled      Cause = cause.Canceled
	CauseTooLarge      Cause = cause.TooLarge
	CauseNoCredentials Cause = cause.NoCredentials
	CauseAuthFailed    Cause = cause.AuthFailed
	CauseRateLimited   Cause = cause.RateLimited
	CauseServerError   Cause = cause.ServerError
	CauseInvalidAnswer Cause = cause.InvalidAnswer
	CauseRejected      Cause = cause.Rejected
	CauseNetworkError  Cause = cause.NetworkError
	// CauseError is an error of no known kind.
	CauseError Cause = cause.Error
)

// Causes that are not Decide failures.
const (
	// CauseNothingLeftOut means every older segment was kept whole, or
	// there was none, so a summary would cover nothing.
	CauseNothingLeftOut Cause = "nothing left out"
	// CauseOverBudget means the user's own words or the tail fill the
	// size budget alone.
	CauseOverBudget Cause = "over budget"
	// CausePairing means a tool call would be separated from its result.
	CausePairing Cause = "pairing"
	// CauseOff means OlderFraction or Base.TailFraction is negative.
	CauseOff Cause = "off"
	// CauseSummaryFailed means the summary request failed.
	CauseSummaryFailed Cause = "summary failed"
)

// fallback is an error that names its Cause.
type fallback struct {
	cause Cause
	err   error
}

func (f *fallback) Error() string { return f.err.Error() }
func (f *fallback) Unwrap() error { return f.err }

// fallBack wraps err with c.
func fallBack(c Cause, err error) error { return &fallback{cause: c, err: err} }

// causeOf names why err made Start fall back. An error without a named
// cause is a Decide failure, named by cause.Of.
func causeOf(err error) Cause {
	if f, ok := errors.AsType[*fallback](err); ok {
		return f.cause
	}
	switch {
	case errors.Is(err, errNothingDropped):
		return CauseNothingLeftOut
	case errors.Is(err, compact.ErrTooLarge):
		return CauseTooLarge
	}
	return Cause(cause.Of(err))
}
