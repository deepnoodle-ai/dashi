package toolcheck

import (
	"context"
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/decide"
)

// unconfirmedError preserves the cause without letting a payload-specific
// failure disable checks for every turn sharing the client.
type unconfirmedError struct {
	cause error
}

func (e *unconfirmedError) Error() string {
	return fmt.Sprintf("toolcheck: command outage unconfirmed: %v", e.cause)
}

func (e *unconfirmedError) Unwrap() error {
	return e.cause
}

func confirmCommandOutage(
	ctx context.Context,
	c *decide.Client,
	key pauseKey,
	a asked,
	cause error,
) (asked, error) {
	if !IsOutage(cause) {
		return a, cause
	}
	if errors.Is(cause, decide.ErrAuth) ||
		errors.Is(cause, decide.ErrNoAPIKey) {
		return a, pauseCommand(ctx, key, cause)
	}
	// A repeated command failure could be a persistent payload bug. Check
	// availability with constant text that the command cannot shape.
	p, err := askOne(
		ctx,
		c,
		map[string]string{"command": "true"},
		commandQuestions,
		decide.WithCallMaxRetries(0),
	)
	a.requests += p.requests
	a.usage.InputTokens += p.usage.InputTokens
	a.usage.OutputTokens += p.usage.OutputTokens
	if ctx.Err() != nil {
		return a, errors.Join(ctx.Err(), cause, err)
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return a, errors.Join(err, cause)
	}
	if IsOutage(err) {
		return a, pauseCommand(ctx, key, cause)
	}
	return a, &unconfirmedError{cause: cause}
}

// pauseCommand returns the shared pause, including on its first check.
// Concurrent confirmations retain their cause without extending the pause.
func pauseCommand(ctx context.Context, key pauseKey, cause error) error {
	pauses.Lock()
	defer pauses.Unlock()
	if err := ctx.Err(); err != nil {
		return errors.Join(err, cause)
	}
	if p := pauses.m[key]; p != nil && now().Before(p.Until) {
		return errors.Join(cause, p)
	}
	return pauseLocked(key, cause)
}
