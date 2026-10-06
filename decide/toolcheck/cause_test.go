package toolcheck_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/decide"
)

func TestCauseOf(t *testing.T) {
	for err, want := range map[error]toolcheck.Cause{
		fmt.Errorf("x: %w", context.DeadlineExceeded):     toolcheck.CauseTimeout,
		toolcheck.ErrTooLarge:                             toolcheck.CauseTooLarge,
		fmt.Errorf("x: %w", decide.ErrNoAPIKey):           toolcheck.CauseNoCredentials,
		decide.ErrAuth:                                    toolcheck.CauseAuthFailed,
		decide.ErrRateLimited:                             toolcheck.CauseRateLimited,
		decide.ErrServer:                                  toolcheck.CauseServerError,
		decide.ErrInvalidAnswer:                           toolcheck.CauseInvalidAnswer,
		decide.ErrValidation:                              toolcheck.CauseRejected,
		&decide.APIError{StatusCode: 400, Message: "Bad"}: toolcheck.CauseRejected,
		errors.New("something else"):                      toolcheck.CauseError,
		&toolcheck.PausedError{Until: time.Now(), Cause: decide.ErrServer}: toolcheck.CauseServerError,
	} {
		if got := toolcheck.CauseOf(err); got != want {
			t.Errorf("CauseOf(%v) = %q; want %q", err, got, want)
		}
	}
}
