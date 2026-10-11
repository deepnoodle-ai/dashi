package feed

import (
	"context"
	"iter"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/recordfollow"
)

// Tail yields committed steps after the cursor until ctx ends, the reader
// fails, or the consumer breaks the range. It includes ending stops and later
// records, unlike Follow. It carries no previews and starts no goroutines.
//
// Tail shares dashi.Follow's cursor checks, paging, wait hints and backoff. It
// yields a read error once, including dashi.ErrCursorAhead or a discontinuity
// matching dashi.ErrRecordInvalid. Negative cursors mean zero. Cancellation
// ends quietly and affects only this reader; readers must honor ctx promptly.
//
// Authorize access and check existence before reading: an unknown turn waits.
// Persist the last applied Seq and handle duplicate application across restart.
// The host owns the lifetime; avoid permanent watchers for historical bills.
// Host ledger changes outside the record require their own notifications.
func Tail(
	ctx context.Context,
	r dashi.StepReader,
	turnID string,
	after int64,
) iter.Seq2[dashi.Step, error] {
	return recordfollow.Follow(
		ctx,
		r,
		turnID,
		after,
		func(step dashi.Step) recordfollow.Position {
			return recordfollow.Position{
				TurnID:  step.TurnID,
				Seq:     step.Seq,
				Ended:   step.Stop != nil && step.Stop.Ended,
				Resting: step.Kind == dashi.StepTurnStopped,
			}
		},
		false,
		dashi.ErrCursorAhead,
		dashi.ErrRecordInvalid,
		recordfollow.Defaults(),
	)
}
