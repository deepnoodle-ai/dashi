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
	ctx context.Context
	// commitCtx is the caller's context, before the turn deadline and the
	// inbox's cancel were added to ctx. Commits and inbox calls use it, so
	// neither of Dashi's own cancellations can break a write in flight.
	commitCtx context.Context
	turn      Turn
	history   []llm.Message
	agent     Agent
	runner    Runner
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
	// listen watches an InboxNotifier for urgent steers and Cancel; nil
	// when the inbox cannot signal.
	listen *steerListener
	// cancelRun ends the run's context with an inboxCancel cause; nil
	// without an inbox.
	cancelRun context.CancelCauseFunc
	// pendingDelivered lists inbox command IDs the next commit records,
	// for CommitRequest.Delivered.
	pendingDelivered []string
	// pendingWarnings and pendingHooks ride on the next committed step, so
	// a warning or hook decision is saved with the step it leads to.
	pendingWarnings     []Warning
	pendingHooks        []HookResult
	compactionUsed      bool
	compactionCandidate *llm.Input
}

// result pairs an RunResult with the recording failure that ended the run.
func (r *runtime) result(out RunResult) (RunResult, error) {
	return out, r.err
}

var errTurnDeadline = errors.New("dashi: turn deadline reached")

func (r *runtime) contextStop() RunResult {
	cause := context.Cause(r.ctx)
	if c, ok := errors.AsType[*inboxCancel](cause); ok {
		return r.cancelStop(c.command)
	}
	if errors.Is(cause, errTurnDeadline) {
		return r.stop(StopLimited, string(LimitDeadline))
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

func (r *runtime) pauseStop() RunResult {
	return r.stop(StopInterrupted, "pause requested")
}

func (r *runtime) failureOrContext(err error) RunResult {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	return r.stop(StopFailed, err.Error())
}

func (r *runtime) commit(step Step) *RunResult {
	if step.Context != nil && len(step.Context.Items) > 0 {
		items := r.skipRecordedItems(step.Context.Items)
		if len(items) == 0 {
			if step.Kind == StepMessagesAdded {
				// Everything was added before; there is nothing to record.
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
		step.Model.Purpose = modelPurpose(r.turn, step.Model.ModelCallID)
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
		stop.Ended = r.endsTurnAfter(stop)
		step.Stop = &stop
	}
	if step.Kind == StepModelCallStarted {
		// The fallback usage counts toward spend before the call is sent.
		// Refuse a model_call_started step that would break a token cap,
		// with either recorder.
		candidate := r.turn
		candidate.Steps = append(slices.Clone(r.turn.Steps), step)
		state, err := replay(candidate)
		if err == nil && incompleteUsageStop(state) != nil {
			err = errors.New("dashi: model call start usage aggregation overflows")
		}
		if err != nil {
			r.err = &CommitError{
				NothingWritten: true,
				Step:           &step,
				Err:            err,
			}
			o := r.current()
			return &o
		}
	}
	if _, err := json.Marshal(step); err != nil {
		r.err = &CommitError{
			NothingWritten: true,
			Step:           &step,
			Err:            err,
		}
		o := r.current()
		return &o
	}
	ctx := r.commitCtx
	if ctx == nil {
		ctx = r.ctx
	}
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
			ModelCall:   r.modelCallCost(step),
			Delivered:   slices.Clone(r.pendingDelivered),
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
	r.pendingDelivered = nil
	if r.emitter.active() || r.opts.streamSink != nil {
		event := Event{TurnID: step.TurnID, Step: &step}
		if step.Model != nil {
			event.ModelCallID = step.Model.ModelCallID
			event.MessageID = step.Model.MessageID
		}
		r.emit(event)
	}
	return nil
}

// endsTurnAfter reports whether recording stop would end the turn. A stop
// changes none of the state terminal reads except the stop itself.
func (r *runtime) endsTurnAfter(stop Stop) bool {
	state, err := replay(r.turn)
	if err != nil {
		// The commit's own record check reports the broken record.
		return false
	}
	return state.stopEnds(stop)
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

func (r *runtime) current() RunResult {
	s, e := replay(r.turn)
	if e != nil {
		if r.err == nil {
			r.err = fmt.Errorf("%w: %w: %v", ErrRecordInvalid, ErrInvalidTurn, e)
		}
		return RunResult{Turn: r.turn.Clone()}
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

func (r *runtime) stop(kind StopKind, reason string) RunResult {
	stop := &Stop{Kind: kind, Reason: reason}
	if kind == StopLimited {
		// A limit kind is recorded even when the counts are unavailable,
		// since whether the stop ends the turn depends on it.
		state, err := replay(r.turn)
		if err != nil {
			state = &replayState{}
		}
		switch LimitKind(reason) {
		case LimitModelCalls:
			stop.Limit = &LimitDetail{
				Kind:  LimitModelCalls,
				Limit: int64(state.modelCallCap),
				Used:  int64(state.ModelCalls),
			}
		case LimitResumes:
			stop.Limit = &LimitDetail{
				Kind:  LimitResumes,
				Limit: int64(max(r.agent.Limits.Resumes, 0)),
				Used:  int64(state.Resumes),
			}
		case LimitProviderPauses:
			stop.Limit = &LimitDetail{
				Kind:  LimitProviderPauses,
				Limit: int64(max(r.agent.Limits.ProviderPauses, 0)),
				Used:  int64(state.ProviderPauses),
			}
		case LimitContextWindow:
			stop.Limit = &LimitDetail{
				Kind:  LimitContextWindow,
				Limit: int64(state.ActiveCompactionWindow),
			}
		case LimitDeadline:
			stop.Limit = &LimitDetail{
				Kind:  LimitDeadline,
				Limit: r.agent.Limits.Deadline.Unix(),
				Used:  time.Now().Unix(),
			}
		}
	}
	return r.stopWith(stop)
}

func (r *runtime) stopWith(stop *Stop) RunResult {
	o := r.commit(Step{Kind: StepTurnStopped, Stop: stop})
	if o != nil {
		return *o
	}
	return r.current()
}

func (r *runtime) stopAfterReply(s *replayState) RunResult {
	if r.ctx.Err() != nil {
		return r.contextStop()
	}
	return r.completeReply(s)
}

// completeReply records the stop after a recorded reply or output.
func (r *runtime) completeReply(s *replayState) RunResult {
	// The reserved final call ends at the model-call limit. After Continue
	// raises the limit, a recorded reply completes the turn.
	if s.finalReplyOnly && s.modelCallCap <= s.ModelCalls {
		return r.stop(StopLimited, string(LimitModelCalls))
	}
	return r.stop(StopCompleted, "")
}

// modelCallCost is the CommitRequest.ModelCall for step: the call it starts
// or finishes, as TurnView.ModelCallCosts will show it, or nil.
func (r *runtime) modelCallCost(step Step) *ModelCallCost {
	if step.Model == nil {
		return nil
	}
	switch step.Kind {
	case StepModelCallStarted:
		return new(ModelCallCostOf(step.Model, nil))
	case StepModelCallFinished, StepModelCallFinishedByHost:
		for i := len(r.turn.Steps) - 1; i >= 0; i-- {
			old := r.turn.Steps[i]
			if old.Kind == StepModelCallStarted && old.Model != nil &&
				old.Model.ModelCallID == step.Model.ModelCallID {
				return new(ModelCallCostOf(old.Model, step.Model))
			}
		}
	}
	return nil
}
