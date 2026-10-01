package delegate

import (
	"context"

	"github.com/deepnoodle-ai/dashi"
)

// RunUntilQuiet runs the session's queue and waits while tasks run. It
// returns when no task is pending and the queue is empty, with the outcome
// of the last turn it ran, or of the session's last turn when it ran none.
// It also returns when a turn stops with anything but dashi.NextNone, since
// that turn needs the host first; its queued items wait
// (dashi.SessionState.Queued counts them). With Config.NoticeReady, it
// propagates AttentionError when only deferred child results remain. The host
// must inspect and resolve those children before the conversation is quiet.
//
// Each ended child turn's notice must reach the session: Tasks puts it
// there when Config.Session is s, or the host's Config.OnTask does, with
// SteerItem(ctx, e.Task.Notice().Steer()), which reaches a running turn, or
// EnqueueItem(ctx, e.Task.Notice().Item()), which waits for the next one.
// Either way a notice that arrives while no turn runs starts a queue turn.
// One Tasks serves one session.
func RunUntilQuiet(
	ctx context.Context,
	s *dashi.Session,
	tasks *Tasks,
	opts ...dashi.RunOption,
) (dashi.Outcome, error) {
	quiet := false
	for {
		out, ran, err := s.RunQueue(ctx, opts...)
		if err != nil || ran && out.Next() != dashi.NextNone {
			return out, err
		}
		switch {
		case ran:
			// The turn may have started tasks of its own.
			quiet = false
			continue
		case quiet:
			return out, nil
		}
		// Wait returns after each report, and reports nothing pending only
		// once OnTask has returned for every ended task, so the notice is
		// on the session by then.
		if quiet, err = tasks.Wait(ctx); err != nil {
			return out, err
		}
	}
}
