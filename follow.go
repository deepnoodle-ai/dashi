package dashi

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"
)

// ErrCursorAhead reports a cursor past a turn's last committed step. It means
// the reader's cursor did not come from this turn's record: a wrong turn ID,
// a read from a lagging replica, or a step the store lost. Retrying with the
// same cursor fails again; tell the client to drop its state and follow from
// zero.
var ErrCursorAhead = errors.New("dashi: cursor is past the turn's last step")

// StepReader is an optional Recorder capability that serves live readers:
// Follow reads a turn's record through it, from any process, while the turn
// runs elsewhere. Implement it over the table the recorder commits to.
//
// StepsAfter returns up to limit committed steps with Seq greater than
// after, in ascending Seq order with no gaps, each with the JSON meaning it
// was committed with. A limit of zero or less means 100. It returns no steps
// and no error when after is the turn's last Seq, or when after is zero and
// the turn has no steps. When after is past the last Seq, including any
// positive after on a turn with no steps, it returns an error matching
// ErrCursorAhead. Lease claims are not steps and never appear.
//
// Seq is assigned by the one worker that owns the turn and each commit waits
// for the previous one, so a plain range query is exact: no reader sees Seq 6
// before 5. Read from the primary; a lagging replica breaks that.
// dashitest.Run checks these rules for any store that implements StepReader.
type StepReader interface {
	StepsAfter(
		ctx context.Context,
		turnID string,
		after int64,
		limit int,
	) ([]Step, error)
}

// StepWaiter is an optional StepReader capability that tells Follow when to
// read again, so new steps arrive without waiting for a poll.
//
// Wait returns nil at once when a step after the cursor exists, and
// otherwise when a step may have been committed since; it returns ctx's
// error when ctx ends first. It is a latency hint, never a delivery path: a
// missed wake costs one poll, and an error makes Follow poll instead.
type StepWaiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}

// Follow intervals. They are variables so tests can shorten them.
var (
	// followPoll is the interval without a StepWaiter, or after one fails.
	followPoll = time.Second
	// followBackstop bounds a wait on a StepWaiter.
	followBackstop = 15 * time.Second
	// followResting applies while the turn rests at a stop that is not
	// terminal, which can last days while it waits for a person.
	followResting = 30 * time.Second
)

// followPage is the page size Follow asks for; a shorter page means Follow
// has caught up. It is a variable so tests can shorten it.
var followPage = 100

// Follow yields the turn's committed steps with Seq greater than after, then
// each new step as it commits. It reads through r, pages until it catches
// up, and then waits on r's StepWaiter, when it has one, or polls.
//
// Follow ends when ctx ends, after yielding a step whose stop is terminal, or
// after yielding an error; ending never affects the turn. A late result of
// work started before a terminal stop can still be recorded after it, so a
// reader that must see every step (accounting, a mirror) keeps its cursor
// and reads again later. A read error, including ErrCursorAhead, is yielded
// once. A step that does not continue the cursor (another turn's step, or a
// gap in Seq) yields an error matching ErrRecordInvalid.
//
// Follow only reads, so it serves any number of readers, each with its own
// cursor, in any process that can reach the store the Runner commits to. A
// negative after means zero. On an unknown turn it waits, so check the turn
// exists (and that the caller may see it) first.
func Follow(
	ctx context.Context,
	r StepReader,
	turnID string,
	after int64,
) iter.Seq2[Step, error] {
	return func(yield func(Step, error) bool) {
		cursor := max(after, 0)
		resting := false
		for ctx.Err() == nil {
			steps, err := r.StepsAfter(ctx, turnID, cursor, followPage)
			if err != nil {
				if ctx.Err() == nil {
					yield(Step{}, err)
				}
				return
			}
			for _, step := range steps {
				if step.TurnID != turnID || step.Seq != cursor+1 {
					yield(Step{}, fmt.Errorf(
						"%w: following turn %q after seq %d, the reader "+
							"returned seq %d of turn %q",
						ErrRecordInvalid,
						turnID,
						cursor,
						step.Seq,
						step.TurnID,
					))
					return
				}
				if !yield(step, nil) {
					return
				}
				cursor = step.Seq
				if step.Stop != nil && step.Stop.Terminal {
					return
				}
				resting = step.Kind == StepStopped
			}
			if len(steps) >= followPage {
				continue
			}
			followWait(ctx, r, turnID, cursor, resting)
		}
	}
}

// followWait returns when the next page is worth reading: after a wake, a
// poll interval, or the end of ctx.
func followWait(
	ctx context.Context,
	r StepReader,
	turnID string,
	cursor int64,
	resting bool,
) {
	w, ok := r.(StepWaiter)
	if !ok {
		sleep(ctx, pollInterval(followPoll, resting))
		return
	}
	wctx, cancel := context.WithTimeout(
		ctx,
		pollInterval(followBackstop, resting),
	)
	err := w.Wait(wctx, turnID, cursor)
	expired := wctx.Err() != nil
	cancel()
	if err != nil && !expired {
		// The hint failed; poll rather than spin on it.
		sleep(ctx, pollInterval(followPoll, resting))
	}
}

func pollInterval(d time.Duration, resting bool) time.Duration {
	if resting {
		return max(d, followResting)
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
