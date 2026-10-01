package dashi

import (
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/tool"
)

// finishResult is the combined BeforeFinish answer. A reason is a veto: the
// answer is unacceptable. Items alone are a nudge: try once more.
type finishResult struct {
	// steers supersede the candidate before the hooks see it.
	steers      []ContextItem
	deliver     []ContextItem
	acceptEmpty bool
	// reason is the first nonempty Reason, bounded; hook is the first hook
	// that turned the answer down.
	reason string
	hook   string
	// noCall means the hooks vetoed the answer and no further model call can
	// happen: the caller records the rejection and stops the turn with
	// StopReasonOutputRejected.
	noCall bool
}

// beforeFinish runs the BeforeFinish chain for a candidate answer. A failing
// hook stops the turn, since BeforeFinish guards what the turn accepts; the
// candidate survives the stop, so Continue asks the hooks again.
func (r *runtime) beforeFinish(plan FinishPlan) (finishResult, *Outcome) {
	var out finishResult
	mark := r.hookMark()
	for _, h := range r.hooks.BeforeFinish {
		p := plan
		p.Turn = snapshotView(plan.Turn)
		p.Output = slices.Clone(plan.Output)
		d, err := callHook(hookContext(r.ctx, plan.Turn), h, p)
		if err == nil {
			for _, item := range d.AddMessages {
				if e := item.Validate(); e != nil {
					err = fmt.Errorf("invalid context item: %w", e)
					break
				}
			}
		}
		if err != nil {
			if r.ctx.Err() != nil {
				r.dropHooks(mark)
				o := r.contextStop()
				return finishResult{}, &o
			}
			if h.FailOpen {
				r.hookFailed(HookBeforeFinish, h.Name, err, true)
				continue
			}
			r.hookFailed(HookBeforeFinish, h.Name, err, false)
			o := r.stopWith(hookFailedStop())
			return finishResult{}, &o
		}
		if len(d.AddMessages) > 0 || d.Reason != "" {
			reason := boundedReason(d.Reason)
			for _, item := range d.AddMessages {
				out.deliver = append(out.deliver, cloneContextItem(item))
			}
			if out.hook == "" {
				out.hook = h.Name
			}
			if out.reason == "" {
				out.reason = reason
			}
			verdict := VerdictAddMessages
			if reason != "" {
				verdict = VerdictReject
			}
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: verdict,
				Reason:  reason,
			})
		}
		if d.AcceptEmpty {
			out.acceptEmpty = true
			r.addHookResult(HookResult{
				Point:   HookBeforeFinish,
				Name:    h.Name,
				Verdict: VerdictAcceptEmpty,
			})
		}
	}
	return out, nil
}

// finishPoint is the one place a candidate final answer is decided, on both
// the final-text path and the dashi_submit_output path. Steering's check for
// waiting steers belongs first here, before the BeforeFinish hooks: a steer
// supersedes a candidate without asking the hooks about it, recorded with
// source sourceSteering.
//
// When the hooks turn the candidate down, the caller records the rejection
// and the model is asked again. When no further normal model call can
// happen, a nudge (items alone) is dropped with a warning and the candidate
// stands, since a finish hook never spends the last answer; a veto (a
// reason) is still recorded, and the turn stops with
// StopReasonOutputRejected instead of accepting the answer.
func (r *runtime) finishPoint(
	s *replayState,
	plan FinishPlan,
) (finishResult, *Outcome) {
	// A steer is a nudge: it supersedes the candidate only when a further
	// model call can happen, and otherwise waits with its source.
	if steers := r.waitingSteers(s); len(steers) > 0 {
		if r.nextCallPossible(s, estimatedItemTokens(steers)) {
			return finishResult{steers: steers}, nil
		}
		r.steersWait(s, len(steers))
	}
	mark := r.hookMark()
	finish, o := r.beforeFinish(plan)
	if o != nil {
		return finishResult{}, o
	}
	if r.ctx.Err() != nil {
		r.dropHooks(mark)
		o := r.contextStop()
		return finishResult{}, &o
	}
	// Items the turn already records are not new: a nudge made only of
	// them is no nudge, and costs no model call.
	finish.deliver = r.skipRecordedItems(finish.deliver)
	if !finish.rejects() {
		return finish, nil
	}
	extra := estimatedItemTokens(finish.deliver) + len(finish.reason)
	if finish.reason != "" {
		// Once the answer is vetoed, the reserve protects nothing, so the
		// reserved final-answer call may answer again.
		finish.noCall = !r.callFits(s, extra, true)
		return finish, nil
	}
	if r.nextCallFits(s, extra) {
		return finish, nil
	}
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code: WarningMessagesDropped,
		Message: fmt.Sprintf(
			"BeforeFinish hook %s asked for another model call, but none "+
				"can happen before the reserved final answer or a limit; "+
				"the answer stands",
			finish.hook,
		),
	})
	return finishResult{acceptEmpty: finish.acceptEmpty}, nil
}

// rejects reports whether the hooks turned the candidate down.
func (f finishResult) rejects() bool {
	return len(f.deliver) > 0 || f.reason != ""
}

// notAccepted is the model-visible text of a turned-down answer.
func notAccepted(reason string) string {
	return "Not accepted: " + reason
}

// sourceSteering is the source a steer records when it turns a submission
// down through rejectSubmission.
const sourceSteering = "steering"

// deliveryDropped warns that context delivered while an answer was kept
// cannot reach the model, because no further model call can happen.
func (r *runtime) deliveryDropped() {
	r.pendingWarnings = append(r.pendingWarnings, Warning{
		Code: WarningMessagesDropped,
		Message: "context delivered after the answer was kept cannot " +
			"reach the model: no further model call can happen, so the " +
			"answer is checked again",
	})
}

// rejectSubmission settles a started dashi_submit_output call that a hook
// or a steer turned down, as failed with the model-visible output
// "Not accepted: <reason>". The items to deliver ride on the same step, so
// a crash cannot record the rejection without them; projection places them
// after the call's result. source names who rejected it for the record,
// such as "hook <name>" or "steering". It is the same started-to-settled
// transition a schema rejection uses, so the next successful submission is
// the one recorded as output.
//
// vetoed records a BeforeFinish veto, which keeps the text of the model
// message that made the call out of TurnView.Reply until a new model result
// arrives.
func (r *runtime) rejectSubmission(
	call tool.Call,
	reason string,
	source string,
	items []ContextItem,
	vetoed bool,
) *Outcome {
	out := tool.Outcome{
		State:  tool.StateFailed,
		Output: new(tool.TextOutput(notAccepted(reason))),
	}
	step := Step{
		Kind: StepToolFinished,
		Tool: &ToolFact{
			Call:    call,
			Outcome: &out,
			Reason:  "not accepted: " + source,
			Vetoed:  vetoed,
		},
	}
	if len(items) > 0 {
		step.Context = &ContextFact{
			Items:    items,
			Steering: source == sourceSteering,
		}
	}
	return r.commit(step)
}
