package dashi

import (
	"context"
	"errors"
	"strconv"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
)

// sendAsk records what asking a held turn found.
type sendAsk struct {
	// allowed is set when every BeforeSend hook allowed the next call.
	allowed bool
}

// heldStop reports whether view rests at a hold: a host limit that does
// not end the turn, such as a balance too low for the next call. Only the
// host can lift it, by letting the call go again.
func heldStop(view TurnView) bool {
	stop := view.Stop
	return stop.Kind == StopLimited && !stop.Ended && !stop.Inferred &&
		stop.Limit != nil && stop.Limit.Kind == LimitHost && !stop.Limit.Ends
}

// askSend asks whether a held turn may send its next model call now. It
// applies the derived Continue as Run would, with the call's options, but
// in memory: its steps are never recorded or shown to the Observer, it
// takes nothing from the inbox, and the run ends once the BeforeModel and
// BeforeSend hooks have answered for the next call, before it is sent and
// before any tool runs. Preparing reads the toolset and any content
// references; Client.Prepare is offline. Waiting steers are not in the
// request it asks about.
//
// It reports false when the turn would stop at the same hold again, so a
// poll of a held turn writes no steps. Otherwise the Continue runs and
// records what happens: the call, or a different stop.
//
// A hold the recorder made (a LimitError) cannot be asked about: the
// hooks allowed that call. Such a hold is retried for real at most once
// per Advance call and per inbox signal (a.recorderRetry).
func (a *advancer) askSend(ctx context.Context, turn Turn, view TurnView) (bool, error) {
	o := a.opts
	o.inbox, o.inboxSet = nil, true
	o.streamSink = nil
	o.delivered = nil
	o.warnings = nil
	ask := &sendAsk{}
	o.asking = ask
	out, err := a.runner.run(ctx, turn, a.derivedContinue(view), o)
	if cerr := ctx.Err(); cerr != nil {
		return false, cerr
	}
	switch {
	case errors.Is(err, ErrHistoryMismatch):
		return true, nil // the Continue reports it
	case err != nil && isRefusal(err):
		return false, nil // the Continue would be refused too
	case err != nil:
		return true, nil // the Continue reports it
	}
	if !ask.allowed {
		return !sameHold(lastStep(out.Turn), view.Stop), nil
	}
	if !holdMadeByHook(turn) {
		if !a.recorderRetry {
			return false, nil
		}
		a.recorderRetry = false
	}
	return true, nil
}

// sameHold reports whether step stops the turn at the hold held: a host
// limit with the same code that does not end the turn.
func sameHold(step *Step, held Stop) bool {
	if step == nil || step.Kind != StepTurnStopped || step.Stop == nil {
		return false
	}
	stop := *step.Stop
	return heldStop(TurnView{Stop: stop}) && stop.Limit.Code == held.Limit.Code
}

// holdMadeByHook reports whether a BeforeModel or BeforeSend hook recorded
// the turn's hold, rather than the recorder refusing the call with a
// LimitError. Advance may repeat the hold's stop step, without its hook
// results, to record inbox input or warnings, so it reads every stop step
// since the last Start or Continue.
func holdMadeByHook(t Turn) bool {
	for i := len(t.Steps) - 1; i >= 0; i-- {
		step := t.Steps[i]
		switch step.Kind {
		case StepTurnStarted, StepTurnContinued:
			return false
		case StepTurnStopped:
			for _, h := range step.HookResults {
				if h.Point == HookBeforeSend && h.Verdict == VerdictLimit ||
					h.Point == HookBeforeModel && h.Verdict == VerdictStop {
					return true
				}
			}
		}
	}
	return false
}

// derivedContinue is the Continue Advance derives from the record, with
// the call's history options.
func (a *advancer) derivedContinue(view TurnView) Continue {
	return Continue{
		CommandID:         continuePrefix + strconv.FormatInt(view.LastSeq, 10),
		History:           llmclone.Messages(a.opts.history),
		OnHistoryMismatch: a.opts.historyMismatch,
	}
}
