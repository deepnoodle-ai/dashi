package toolcheck

import (
	"errors"

	"github.com/deepnoodle-ai/dashi/decide/internal/cause"
)

// Cause names, in a few words, why a check could not answer.
type Cause string

const (
	CauseTimeout       Cause = cause.Timeout
	CauseCanceled      Cause = cause.Canceled
	CauseTooLarge      Cause = cause.TooLarge
	CauseNoCredentials Cause = cause.NoCredentials
	CauseAuthFailed    Cause = cause.AuthFailed
	CauseRateLimited   Cause = cause.RateLimited
	CauseServerError   Cause = cause.ServerError
	CauseInvalidAnswer Cause = cause.InvalidAnswer
	// CauseRejected is a request Decide refused, such as one that failed
	// its validation.
	CauseRejected     Cause = cause.Rejected
	CauseNetworkError Cause = cause.NetworkError
	// CauseError is an error of no known kind.
	CauseError Cause = cause.Error
)

// CauseOf names why err stopped a check, from Check or from a turnwatch
// Check. A *PausedError names the outage that started the pause.
func CauseOf(err error) Cause {
	if errors.Is(err, ErrTooLarge) {
		return CauseTooLarge
	}
	return Cause(cause.Of(err))
}
