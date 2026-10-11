package dashi

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/recordfollow"
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
// StepsAfter returns up to limit committed steps of the named turn with Seq
// greater than after, in ascending Seq order with no gaps, each with the
// JSON meaning it was committed with. Callers pass a positive limit. It
// returns no steps and no error when after is the turn's last Seq, or when
// after is zero and the turn has no steps. When after is past the last Seq,
// including any positive after on a turn with no steps, it returns an error
// matching ErrCursorAhead. Lease claims are not steps and never appear.
//
// Seq is assigned by the one worker that owns the turn and each commit waits
// for the previous one, so a plain range query is exact: no reader sees Seq 6
// before 5. Read from the primary; a lagging replica breaks that.
// dashitest.Run checks these rules for any store that implements StepReader.
//
// StepsAfter must return promptly once ctx ends: readers such as
// feed.Follow wait for their read to finish before they return.
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
// error when ctx ends first. It is a latency hint, never a delivery path:
// Follow reads again at least every 15 seconds while it waits, so a missed
// wake delays a step by up to that long. A Wait that also sees commits from
// other processes should notice them within about a second. An error makes
// Follow poll instead. Like StepsAfter, Wait must return promptly once ctx
// ends.
type StepWaiter interface {
	Wait(ctx context.Context, turnID string, after int64) error
}

// Finishes returns the ID of the live previews this step replaces: the
// MessageID of a finished model call (including one the host resolved),
// whose deltas it replaces, or the call ID of a finished tool call, whose
// progress it ends. It returns "" for every other step, including
// model_call_started, which carries the same MessageID as the deltas that
// follow it but finishes nothing.
//
// A handler that merges previews with Follow's steps drops a preview whose
// MessageID (for a delta) or CallID (for progress) a sent step finished.
func (s Step) Finishes() string {
	switch {
	case (s.Kind == StepModelCallFinished || s.Kind == StepModelCallFinishedByHost) &&
		s.Model != nil:
		return s.Model.MessageID
	case s.Kind == StepToolFinished && s.Tool != nil:
		return s.Tool.Call.ID
	}
	return ""
}

// Follow intervals. They are variables so tests can shorten them.
var (
	// followPoll is the interval without a StepWaiter, or after one fails.
	followPoll = recordfollow.Defaults().Poll
	// followBackstop bounds a wait on a StepWaiter.
	followBackstop = recordfollow.Defaults().Backstop
	// followResting applies while the turn rests at a stop that did not
	// end it, which can last days while it waits for a person.
	followResting = recordfollow.Defaults().Resting
	// followFloor is the pause after a wake that found nothing new, so a
	// waiter that reports steps too eagerly cannot make Follow spin.
	followFloor = recordfollow.Defaults().Floor
	// followPage is the page size Follow asks for; a shorter page means
	// Follow has caught up.
	followPage = recordfollow.Defaults().Page
)

// Follow yields the turn's committed steps with Seq greater than after, then
// each new step as it commits. It reads through r, pages until it catches
// up, and then waits on r's StepWaiter, when it has one, or polls.
//
// Follow ends when ctx ends, after yielding a step whose stop ended the turn
// (Stop.Ended), or after yielding an error; ending never affects the turn.
// When the step at the cursor is itself such a stop, Follow ends at once
// without yielding anything, so a reader that reconnects after the end is
// not held open. A late result of work started before the ending stop can
// still be recorded after it; a reader that must see every step
// (accounting, a mirror) uses package feed's Tail, or reads later results with
// StepsAfter from the stop's Seq. A read error, including
// ErrCursorAhead, is yielded once. A step that does not continue the cursor
// (another turn's step, or a gap in Seq) yields an error matching
// ErrRecordInvalid.
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
	return recordfollow.Follow(
		ctx,
		r,
		turnID,
		after,
		func(step Step) recordfollow.Position {
			return recordfollow.Position{
				TurnID:  step.TurnID,
				Seq:     step.Seq,
				Ended:   step.Stop != nil && step.Stop.Ended,
				Resting: step.Kind == StepTurnStopped,
			}
		},
		true,
		ErrCursorAhead,
		ErrRecordInvalid,
		recordfollow.Bounds{
			Page:     followPage,
			Poll:     followPoll,
			Backstop: followBackstop,
			Resting:  followResting,
			Floor:    followFloor,
		},
	)
}

func pollInterval(d time.Duration, resting bool) time.Duration {
	return recordfollow.Interval(d, followResting, resting)
}
