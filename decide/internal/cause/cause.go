// Package cause names, in a few words, why a Decide request failed, so
// the decide packages record and report a failure the same way.
package cause

import (
	"context"
	"errors"
	"net"

	"github.com/deepnoodle-ai/decide"
)

// The words Of returns. toolcheck exports them as its Cause constants.
const (
	Timeout       = "timeout"
	Canceled      = "canceled"
	TooLarge      = "too large"
	NoCredentials = "no credentials"
	AuthFailed    = "auth failed"
	RateLimited   = "rate limited"
	ServerError   = "server error"
	InvalidAnswer = "invalid answer"
	Rejected      = "rejected"
	NetworkError  = "network error"
	Error         = "error"
)

// Of names why err stopped a Decide request. It follows wrapped errors,
// so a pause error names the error that started it. An error it does not
// know is Error.
func Of(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, decide.ErrNoAPIKey):
		return NoCredentials
	case errors.Is(err, decide.ErrAuth):
		return AuthFailed
	case errors.Is(err, decide.ErrRateLimited):
		return RateLimited
	case errors.Is(err, decide.ErrServer):
		return ServerError
	case errors.Is(err, decide.ErrInvalidAnswer):
		return InvalidAnswer
	case errors.Is(err, decide.ErrValidation), errors.Is(err, decide.ErrInvalidRequest):
		return Rejected
	}
	if _, ok := errors.AsType[*decide.APIError](err); ok {
		return Rejected
	}
	if ne, ok := errors.AsType[net.Error](err); ok {
		if ne.Timeout() {
			return Timeout
		}
		return NetworkError
	}
	return Error
}
