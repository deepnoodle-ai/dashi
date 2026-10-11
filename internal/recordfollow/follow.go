// Package recordfollow shares cursor validation and waiting between record
// readers. It knows no public Dashi types and starts no goroutines.
package recordfollow

import (
	"context"
	"fmt"
	"iter"
	"time"
)

// Reader supplies committed entries in sequence.
type Reader[T any] interface {
	StepsAfter(context.Context, string, int64, int) ([]T, error)
}

// Position is the small part of a record needed for reading and waiting.
type Position struct {
	TurnID  string
	Seq     int64
	Ended   bool
	Resting bool
}

// Bounds preserves the existing reader's private page and wait controls.
type Bounds struct {
	Page     int
	Poll     time.Duration
	Backstop time.Duration
	Resting  time.Duration
	Floor    time.Duration
}

// Defaults returns the common reader bounds. Callers may shorten them in tests.
func Defaults() Bounds {
	return Bounds{
		Page:     100,
		Poll:     time.Second,
		Backstop: 15 * time.Second,
		Resting:  30 * time.Second,
		Floor:    50 * time.Millisecond,
	}
}

// Follow shares one loop with a choice of completion policy. info exposes only
// cursor and stop fields; ahead and invalid retain the caller's error identities.
func Follow[T any](
	ctx context.Context,
	r Reader[T],
	turnID string,
	after int64,
	info func(T) Position,
	stopAtEnd bool,
	ahead, invalid error,
	b Bounds,
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		cursor := max(after, 0)
		// Read the cursor's row once to validate it and learn stop state.
		known := cursor > 0
		resting, woke := false, false
		for ctx.Err() == nil {
			from := cursor
			if known {
				from = cursor - 1
			}
			steps, err := r.StepsAfter(ctx, turnID, from, b.Page)
			if err != nil {
				if ctx.Err() == nil {
					yield(zero, err)
				}
				return
			}
			full := len(steps) >= b.Page
			if known {
				known = false
				if len(steps) == 0 {
					yield(zero, fmt.Errorf(
						"%w: turn %q has no step %d", ahead, turnID, cursor,
					))
					return
				}
				last := info(steps[0])
				if last.TurnID != turnID || last.Seq != cursor {
					yield(zero, discontinuous(invalid, turnID, cursor-1, last))
					return
				}
				if stopAtEnd && last.Ended {
					return
				}
				resting = last.Resting
				steps = steps[1:]
			}
			for _, step := range steps {
				position := info(step)
				if position.TurnID != turnID || position.Seq != cursor+1 {
					yield(zero, discontinuous(invalid, turnID, cursor, position))
					return
				}
				if !yield(step, nil) {
					return
				}
				cursor = position.Seq
				if stopAtEnd && position.Ended {
					return
				}
				resting = position.Resting
			}
			if full {
				continue
			}
			if woke && len(steps) == 0 {
				sleep(ctx, b.Floor)
			}
			woke = wait(ctx, r, turnID, cursor, resting, b)
		}
	}
}

func discontinuous(cause error, turnID string, cursor int64, step Position) error {
	return fmt.Errorf(
		"%w: following turn %q after seq %d, the reader returned seq %d "+
			"of turn %q", cause, turnID, cursor, step.Seq, step.TurnID,
	)
}

// wait reports whether a waiter woke the reader before its backstop.
func wait[T any](
	ctx context.Context,
	r Reader[T],
	turnID string,
	cursor int64,
	resting bool,
	b Bounds,
) bool {
	w, ok := r.(interface {
		Wait(context.Context, string, int64) error
	})
	if !ok {
		sleep(ctx, Interval(b.Poll, b.Resting, resting))
		return false
	}
	wctx, cancel := context.WithTimeout(
		ctx, Interval(b.Backstop, b.Resting, resting),
	)
	err := w.Wait(wctx, turnID, cursor)
	expired := wctx.Err() != nil
	cancel()
	if err != nil && !expired {
		// A failed latency hint must not make the reader spin.
		sleep(ctx, Interval(b.Poll, b.Resting, resting))
		return false
	}
	return err == nil && !expired
}

// Interval slows polling while the last record is a stop.
func Interval(d, restingInterval time.Duration, resting bool) time.Duration {
	if resting {
		return max(d, restingInterval)
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
