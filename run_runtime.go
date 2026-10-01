package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

type runtime struct {
	ctx     context.Context
	turn    Turn
	history []llm.Message
	agent   Agent
	runner  Runner
	// implicitRecorder is true when the runner had no Recorder and Run made a
	// private one.
	implicitRecorder bool
	opts             runOptions
	emitter          *emitter
	// err is the recording failure that ended the run, if any.
	err error
	// hooks are the Runner's hooks followed by this call's WithHooks.
	hooks Hooks
	// activeHooks is recorded on a Start or Continue step, and a Continue
	// warns when it differs from lastHooks, the previous one's.
	activeHooks *ActiveHooks
	lastHooks   *ActiveHooks
	// steers tracks the steers this run fetched, for acknowledgment and
	// release.
	steers steerState
	// listen watches a SteeringNotifier source for urgent steers; nil when
	// the source cannot signal.
	listen *steerListener
	// pendingWarnings and pendingHooks ride on the next committed step, so
	// a warning or hook answer is saved with the step it leads to.
	pendingWarnings     []Warning
	pendingHooks        []HookResult
	compactionUsed      bool
	compactionCandidate *llm.Input
}

// result pairs an Outcome with the recording failure that ended the run.
func (r *runtime) result(out Outcome) (Outcome, error) {
	return out, r.err
}

var errTurnDeadline = errors.New("dashi: turn deadline reached")

func (r *runtime) contextStop() Outcome {
	cause := context.Cause(r.ctx)
	if errors.Is(cause, errTurnDeadline) {
		return r.stop(StopLimited, "deadline")
	}
	if cause == nil {
		cause = r.ctx.Err()
	}
	if cause == nil {
		cause = context.Canceled
	}
	if r.runner.OnContextDone == ContextCancel {
		return r.stop(StopCanceled, cause.Error())
	}
	return r.stop(StopInterrupted, cause.Error())
}

func (r *runtime) pauseRequested() bool {
	return pauseClosed(r.opts.pause)
}

func (r *runtime) pauseStop() Outcome {
	return r.stop(StopInterrupted, "pause requested")
}

func (r *runtime) failureOrContext(err error) Outcome {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	return r.stop(StopFailed, err.Error())
}

func (r *runtime) commit(step Step) *Outcome {
	if step.Context != nil && len(step.Context.Items) > 0 {
		items := r.skipRecordedItems(step.Context.Items)
		if len(items) == 0 {
			if step.Kind == StepMessagesAdded {
				// Everything was delivered before; there is nothing to record.
				return nil
			}
			step.Context = nil
		} else {
			fact := *step.Context
			fact.Items = items
			step.Context = &fact
		}
	}
	if step.Model != nil && (step.Kind == StepModelCallFinished || step.Kind == StepModelCallFinishedByHost) {
		step.Model.Purpose = modelPurpose(r.turn, step.Model.AttemptID)
	}
	if recordsActiveHooks(step) {
		step.ActiveHooks = r.activeHooks
		if step.Kind != StepTurnStarted &&
			!canonjson.SameValue(r.lastHooks, r.activeHooks) {
			step.Warnings = append(step.Warnings, Warning{
				Code:    WarningHooksChanged,
				Message: "this command's Runner hooks differ from the previous command's",
			})
		}
	}
	step.Warnings = append(step.Warnings, r.pendingWarnings...)
	step.HookResults = append(step.HookResults, r.pendingHooks...)
	step.Version = 1
	step.TurnID = r.turn.ID
	step.Seq = int64(len(r.turn.Steps) + 1)
	step.At = time.Now().UTC()
	if step.Kind == StepTurnStopped && step.Stop != nil {
		stop := *step.Stop
		stop.Terminal = r.terminalAfter(stop)
		step.Stop = &stop
	}
	if step.Kind == StepModelCallStarted {
		// The fallback usage joins accounting before dispatch. Refuse an
		// intent that would invalidate a token cap with either recorder.
		candidate := r.turn
		candidate.Steps = append(slices.Clone(r.turn.Steps), step)
		state, err := replay(candidate)
		if err == nil && incompleteUsageStop(state) != nil {
			err = errors.New("dashi: model intent usage aggregation overflows")
		}
		if err != nil {
			r.err = &CommitError{
				Persistence: PersistenceRejected,
				Step:        &step,
				Err:         err,
			}
			o := r.current()
			return &o
		}
	}
	if _, err := json.Marshal(step); err != nil {
		r.err = &CommitError{
			Persistence: PersistenceRejected,
			Step:        &step,
			Err:         err,
		}
		o := r.current()
		return &o
	}
	ctx := r.ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	err := r.runner.Recorder.Commit(
		ctx,
		CommitRequest{
			Step:        step,
			ExpectedSeq: step.Seq - 1,
			Owner:       r.opts.owner,
		},
	)
	if err != nil {
		r.err = commitError(step, err)
		o := r.current()
		return &o
	}
	r.turn.Steps = append(r.turn.Steps, cloneStep(step))
	r.listen.committed(step)
	r.pendingWarnings = nil
	r.pendingHooks = nil
	if r.emitter.active() || r.opts.streamSink != nil {
		event := Event{TurnID: step.TurnID, Step: &step}
		if step.Model != nil {
			event.AttemptID = step.Model.AttemptID
			event.MessageID = step.Model.MessageID
		}
		r.emit(event)
	}
	return nil
}

// terminalAfter reports whether recording stop would end the turn. A stop
// changes none of the state terminal reads except the stop itself.
func (r *runtime) terminalAfter(stop Stop) bool {
	state, err := replay(r.turn)
	if err != nil {
		// The commit's own replay check reports the broken record.
		return false
	}
	return state.stopTerminal(stop)
}

// emit stamps event.Seq with the number of recorded steps, so a step event
// carries its own Seq and a preview the Seq of the step it follows. That is
// exact only because commit and emit both run on the run goroutine; never
// emit from a tool goroutine or commit from another goroutine
// (TestEventSeqPlacesPreviews checks this).
func (r *runtime) emit(event Event) {
	event.Seq = int64(len(r.turn.Steps))
	if r.emitter.active() {
		if event.Progress != nil {
			r.emitter.sendProgress(cloneEvent(event))
		} else {
			r.emitter.send(cloneEvent(event))
		}
	}
	if r.opts.streamSink != nil {
		r.opts.streamSink(cloneEvent(event))
	}
}

func cloneEvent(event Event) Event {
	if event.Step != nil {
		step := cloneStep(*event.Step)
		event.Step = &step
	}
	if event.Progress != nil {
		event.Progress = cloneToolProgress(event.Progress)
	}
	if event.Delta != nil {
		delta := *event.Delta
		if delta.Content != nil {
			content := delta.Content.Clone()
			delta.Content = &content
		}
		if delta.Usage != nil {
			delta.Usage = delta.Usage.Copy()
		}
		event.Delta = &delta
	}
	return event
}

func (r *runtime) current() Outcome {
	s, e := replay(r.turn)
	if e != nil {
		if r.err == nil {
			r.err = fmt.Errorf("%w: %w: %v", ErrRecordInvalid, ErrInvalidTurn, e)
		}
		return Outcome{Turn: r.turn.Clone()}
	}
	return outcome(r.turn, s)
}

func hasStartedTool(s *replayState) bool {
	for _, view := range s.Tools {
		if view.Status == ToolStarted {
			return true
		}
	}
	return false
}

func (r *runtime) stop(kind StopKind, reason string) Outcome {
	stop := &Stop{Kind: kind, Reason: reason}
	if kind == StopLimited {
		// A limit kind is recorded even when the counts are unavailable,
		// since whether the stop ends the turn depends on it.
		state, err := replay(r.turn)
		if err != nil {
			state = &replayState{}
		}
		switch reason {
		case "model_attempts":
			stop.Limit = &LimitDetail{
				Kind:  LimitModelAttempts,
				Limit: int64(state.attemptCap),
				Used:  int64(state.ModelAttempts),
			}
		case "interruptions":
			stop.Limit = &LimitDetail{
				Kind:  LimitInterruptions,
				Limit: int64(max(r.agent.Limits.Interruptions, 0)),
				Used:  int64(state.Interruptions),
			}
		case "provider_pauses":
			stop.Limit = &LimitDetail{
				Kind:  LimitProviderPauses,
				Limit: int64(max(r.agent.Limits.ProviderPauses, 0)),
				Used:  int64(state.ProviderPauses),
			}
		case "deadline":
			stop.Limit = &LimitDetail{
				Kind:  LimitDeadline,
				Limit: r.agent.Limits.Deadline.Unix(),
				Used:  time.Now().Unix(),
			}
		}
	}
	return r.stopWith(stop)
}

func (r *runtime) stopWith(stop *Stop) Outcome {
	o := r.commit(Step{Kind: StepTurnStopped, Stop: stop})
	if o != nil {
		return *o
	}
	return r.current()
}

func (r *runtime) stopAfterAnswer(s *replayState) Outcome {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	// The reserved final call ends at the model-call limit. After Continue
	// raises the limit, a recorded answer completes the turn.
	if s.finalAnswerOnly && s.attemptCap <= s.ModelAttempts {
		return r.stop(StopLimited, "model_attempts")
	}
	return r.stop(StopCompleted, "")
}
