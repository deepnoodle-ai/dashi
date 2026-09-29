package dashi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

// toolResult is what the dispatcher observed from one executor call.
// When the executor panicked, panicked holds the recorded reason and outcome
// and err are meaningless.
type toolResult struct {
	call     tool.Call
	outcome  tool.Outcome
	err      error
	panicked string
}

// maxPanicReason bounds the panic value and stack kept on the record, which
// is persisted with every turn.
const maxPanicReason = 4 << 10

func (r *runtime) tools(s *replayState) (Outcome, bool) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	results := make(chan toolResult, len(s.callOrder))
	running := 0
	progress := newProgressHub(r.emitter.active() || r.opts.streamSink != nil)
	// fatal holds the stop reason of the first result that ends the turn.
	// Once set, no further call starts; running calls still settle.
	fatal := ""
	settle := func(v toolResult) *Outcome {
		if ctx.Err() != nil {
			o := r.contextStop()
			return &o
		}
		// The call's slot is closed, so this last snapshot is final.
		r.emitToolProgress(progress.take(v.call.ID, true))
		step, stop := toolSettlement(v)
		if step.Kind == StepToolSettled {
			r.afterTool(ctx, step.Tool)
		}
		if o := r.commit(step); o != nil {
			return o
		}
		if fatal == "" {
			fatal = stop
		}
		return nil
	}
	drainOne := func() *Outcome {
		if ctx.Err() != nil {
			o := r.contextStop()
			return &o
		}
		for {
			select {
			case v := <-results:
				running--
				return settle(v)
			case <-progress.readyChan():
				r.emitProgress(progress, s.callOrder)
			case <-ctx.Done():
				o := r.contextStop()
				return &o
			}
		}
	}
	paused := false
	for _, id := range s.callOrder {
		if ctx.Err() != nil {
			return r.contextStop(), true
		}
		if fatal != "" {
			break
		}
		state, _ := replay(r.turn)
		v := state.Tools[id]
		if v.Status == ToolSettled ||
			v.Status == ToolWaiting ||
			v.Status == ToolApprovalPending ||
			v.Status == ToolUncertain {
			continue
		}
		if v.Status == ToolDenied {
			reason := v.Verdict.Reason
			if v.Approval != nil {
				reason = v.Approval.Reason
			}
			out := tool.Outcome{
				State:  tool.StateNotExecuted,
				Output: new(tool.TextOutput(denialText(reason))),
			}
			if o := r.commit(Step{
				Kind: StepToolSettled,
				Tool: &ToolFact{
					Call:    v.Call,
					Outcome: &out,
					Reason:  reason,
				},
			}); o != nil {
				return *o, true
			}
			continue
		}
		if v.Status == ToolStarted {
			if v.Call.Name == SubmitOutputTool {
				if o := r.settleOutputSubmission(v.Call); o != nil {
					return *o, true
				}
				continue
			}
			if !safeRepeat(v.Definition) {
				fact := &ToolFact{
					Call:   v.Call,
					Reason: "previous execution may have taken effect",
				}
				if o := r.commit(Step{Kind: StepToolSettled, Tool: fact}); o != nil {
					return *o, true
				}
				continue
			}
		}
		if r.pauseRequested() {
			paused = true
			break
		}
		if v.Status == ToolRequested {
			if v.Definition.Name == "" {
				// A bound tool the call did not offer, such as one a hook
				// hid, is unavailable rather than unknown.
				text, reason := "Unknown tool.", "unknown tool"
				if state.hiddenAt(id, v.Call.Name) {
					text, reason = "This tool is not available now.", "tool unavailable"
				}
				o := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput(text)),
				}
				if x := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{Call: v.Call, Outcome: &o, Reason: reason},
				}); x != nil {
					return *x, true
				}
				continue
			}
			if v.Call.Name == SubmitOutputTool &&
				len(v.Call.Args) > maxOutputValueBytes {
				out := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput("Output rejected because it is too large. Return a smaller object and submit again.")),
				}
				if x := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{
						Call:    v.Call,
						Outcome: &out,
						Reason:  "output exceeds size limit",
					},
				}); x != nil {
					return *x, true
				}
				continue
			}
			if err := validateToolCall(v.Definition, v.Call); err != nil {
				message := "Invalid tool arguments: " + err.Error()
				if v.Call.Name == SubmitOutputTool {
					message = "Output rejected: " + boundedOutputError(err) + ". Correct it and call " + SubmitOutputTool + " again."
				}
				out := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput(message)),
				}
				if x := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{
						Call:    v.Call,
						Outcome: &out,
						Reason:  "invalid arguments",
					},
				}); x != nil {
					return *x, true
				}
				continue
			}
			if v.Call.Name == SubmitOutputTool {
				batchCalls := 0
				for _, callID := range state.callOrder {
					if state.callAttempts[callID] == state.callAttempts[id] {
						batchCalls++
					}
				}
				if batchCalls != 1 {
					out := tool.Outcome{
						State:  tool.StateNotExecuted,
						Output: new(tool.TextOutput("Submit output in a separate model response after other tool calls finish.")),
					}
					if x := r.commit(Step{
						Kind: StepToolSettled,
						Tool: &ToolFact{
							Call:    v.Call,
							Outcome: &out,
							Reason:  "submission shared a tool batch",
						},
					}); x != nil {
						return *x, true
					}
					continue
				}
			}
			// An urgent steer skips the call before any hook sees it. The
			// checks above run first, so a call that fails them reports
			// that failure. A submission is never skipped: the finish
			// point decides a waiting steer for it.
			skip := v.Call.Name != SubmitOutputTool && r.steerSkips(state, id)
			if ctx.Err() != nil {
				return r.contextStop(), true
			}
			if skip {
				if o := r.skipForSteer(v.Call); o != nil {
					return *o, true
				}
				continue
			}
			call := v.Call.Clone()
			verdict := ToolVerdict{
				Kind:     DecisionAllow,
				Revision: "dashi-output-v1",
			}
			if call.Name != SubmitOutputTool {
				var o *Outcome
				call, verdict, o = r.authorizeTool(ctx, v.Definition, call)
				if o != nil {
					return *o, true
				}
			}
			if o := r.commit(Step{
				Kind: StepToolDecision,
				Tool: &ToolFact{
					Call:       call,
					Definition: v.Definition,
					Verdict:    verdict,
				},
			}); o != nil {
				return *o, true
			}
			v.Call = call
			v.Verdict = verdict
			if verdict.Kind == DecisionDeny {
				out := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput(denialText(verdict.Reason))),
				}
				if o := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{Call: call, Outcome: &out, Reason: verdict.Reason},
				}); o != nil {
					return *o, true
				}
				continue
			}
			if verdict.Kind == DecisionDefer {
				continue
			}
		}
		if ctx.Err() != nil {
			return r.contextStop(), true
		}
		if v.Call.Name == SubmitOutputTool {
			if o := r.commit(Step{
				Kind: StepToolStarted,
				Tool: &ToolFact{Call: v.Call, Definition: v.Definition},
			}); o != nil {
				return *o, true
			}
			if o := r.settleOutputSubmission(v.Call); o != nil {
				return *o, true
			}
			continue
		}
		// Each check below, just before a call starts, covers a steer that
		// arrived during BeforeTool, while the call waited for a slot, or
		// on resume; the call is allowed but has not started. Handing a
		// call to the host is its start. A check can fetch from the source;
		// a run that ended meanwhile stops before the start, as it does when
		// it ends during BeforeTool.
		if v.Definition.Kind == tool.KindExternal {
			skip := r.steerSkips(state, id)
			if ctx.Err() != nil {
				return r.contextStop(), true
			}
			if skip {
				if o := r.skipForSteer(v.Call); o != nil {
					return *o, true
				}
				continue
			}
			wait := tool.Outcome{
				State: tool.StateWaiting,
				Wait:  &tool.Wait{Reason: "external execution"},
			}
			if o := r.commit(Step{
				Kind: StepToolWaiting,
				Tool: &ToolFact{Call: v.Call, Outcome: &wait},
			}); o != nil {
				return *o, true
			}
			continue
		}
		if v.Definition.Kind == tool.KindProviderHosted {
			return r.stop(StopFailed, "hosted tool cannot execute locally"), true
		}
		if r.runner.Tools == nil {
			return r.stop(StopFailed, "tool binding unavailable"), true
		}
		bound, err := r.runner.Tools.Tools(ctx)
		if err != nil {
			return r.failureOrContext(err), true
		}
		if ctx.Err() != nil {
			return r.contextStop(), true
		}
		var ex tool.Executor
		for _, candidate := range bound {
			if candidate.Definition.Name == v.Call.Name && sameJSONValue(
				candidate.Definition,
				v.Definition,
			) {
				ex = candidate.Executor
				break
			}
		}
		if ex == nil {
			return r.stop(StopFailed, "tool binding changed or unavailable"), true
		}
		if running >= r.agent.Limits.ToolConcurrency {
			if o := drainOne(); o != nil {
				cancel()
				return *o, true
			}
			if fatal != "" {
				break
			}
		}
		if ctx.Err() != nil {
			return r.contextStop(), true
		}
		if r.pauseRequested() {
			paused = true
			break
		}
		if v.Status != ToolStarted {
			skip := r.steerSkips(state, id)
			if ctx.Err() != nil {
				return r.contextStop(), true
			}
			if skip {
				if o := r.skipForSteer(v.Call); o != nil {
					cancel()
					return *o, true
				}
				continue
			}
			fact := &ToolFact{Call: v.Call, Definition: v.Definition}
			if o := r.commit(Step{Kind: StepToolStarted, Tool: fact}); o != nil {
				cancel()
				return *o, true
			}
		}
		if ctx.Err() != nil {
			return r.contextStop(), true
		}
		running++
		call := v.Call.Clone()
		slot := progress.open(call)
		go func() {
			results <- execute(ctx, ex, call, slot)
		}()
	}
	for running > 0 {
		if o := drainOne(); o != nil {
			cancel()
			return *o, true
		}
	}
	if ctx.Err() != nil {
		return r.contextStop(), true
	}
	state, _ := replay(r.turn)
	for _, id := range state.callOrder {
		v := state.Tools[id]
		if v.Status == ToolUncertain {
			return r.stop(StopUncertain, "tool outcome needs reconciliation"), true
		}
	}
	// An uncertain call outranks a fatal one: Continue is refused until the
	// host reconciles it, and the fatal failure stays on its settled step.
	if fatal != "" {
		return r.stop(StopFailed, fatal), true
	}
	if paused {
		return r.pauseStop(), true
	}
	if hasOpenTools(state) {
		return r.stop(StopWaiting, "tool result or approval pending"), true
	}
	if r.pauseRequested() {
		return r.pauseStop(), true
	}
	return Outcome{}, false
}

// execute runs one call and always yields a result, so a panicking executor
// cannot leave the dispatcher waiting on results with running overcounted.
// This single dispatch path serves both sequential and parallel calls. A
// panic is recovered, not re-raised: the call may have taken effect before it
// panicked, so its result is unknown and the runner records it as uncertain
// rather than running it again. The call's progress slot closes before the
// result is sent, even on panic, so no report follows the settlement.
func execute(
	ctx context.Context,
	ex tool.Executor,
	call tool.Call,
	slot *progressSlot,
) (result toolResult) {
	result.call = call
	ctx = tool.WithReporter(ctx, slot.reporter())
	defer slot.close()
	defer func() {
		if v := recover(); v != nil {
			result = toolResult{call: call, panicked: panicReason(v)}
		}
	}()
	result.outcome, result.err = ex.Execute(ctx, call.Clone())
	return result
}

// toolSettlement converts an observed executor result into the step to record
// and, when the result ends the turn, the StopFailed reason. It applies the
// contract in design/decisions.md entry 1: an error is a failure the model
// sees, tool.ErrFatal also stops the turn, and uncertainty is reserved for a
// result the runner did not observe. An invalid outcome was observed, so it is
// a fatal executor bug, not uncertainty.
func toolSettlement(v toolResult) (Step, string) {
	fact := &ToolFact{Call: v.call}
	if v.panicked != "" {
		// No outcome records the call as uncertain.
		fact.Reason = v.panicked
		return Step{Kind: StepToolSettled, Tool: fact}, ""
	}
	err := v.err
	if err == nil {
		if e := v.outcome.Validate(); e != nil {
			err = tool.Fatal(fmt.Errorf("%w: %w", tool.ErrInvalidOutcome, e))
		}
	}
	if err == nil {
		fact.Outcome = &v.outcome
		kind := StepToolSettled
		switch v.outcome.State {
		case tool.StateWaiting, tool.StateDetached:
			kind = StepToolWaiting
		}
		return Step{Kind: kind, Tool: fact}, ""
	}
	fact.Reason = err.Error()
	text := fact.Reason
	if text == "" {
		text = "Tool failed."
	}
	fact.Outcome = &tool.Outcome{
		State:  tool.StateFailed,
		Output: new(tool.TextOutput(text)),
	}
	step := Step{Kind: StepToolSettled, Tool: fact}
	switch {
	case errors.Is(err, tool.ErrInvalidOutcome):
		return step, StopReasonToolOutcomeInvalid
	case errors.Is(err, tool.ErrFatal):
		return step, StopReasonToolFatal
	}
	return step, ""
}

// panicReason records the panic value and a bounded stack, prefixed so hosts
// can recognize a recovered panic among uncertain calls.
func panicReason(v any) string {
	reason := fmt.Sprintf("panic: %v\n%s", v, debug.Stack())
	if len(reason) > maxPanicReason {
		reason = reason[:maxPanicReason]
	}
	return strings.ToValidUTF8(reason, "\uFFFD")
}

func (r *runtime) settleOutputSubmission(call tool.Call) *Outcome {
	state, replayErr := replay(r.turn)
	if replayErr != nil {
		out := r.stop(StopFailed, replayErr.Error())
		return &out
	}
	schema, err := compileOutputSchema(state.OutputSchema)
	if err == nil {
		err = validateOutputValue(schema, call.Args)
	}
	if err != nil {
		message := fmt.Sprintf(
			"Output rejected: %s. Correct it and call %s again.",
			boundedOutputError(err),
			SubmitOutputTool,
		)
		out := tool.Outcome{
			State:  tool.StateFailed,
			Output: new(tool.TextOutput(message)),
		}
		return r.commit(Step{
			Kind: StepToolSettled,
			Tool: &ToolFact{
				Call:    call,
				Outcome: &out,
				Reason:  "output schema rejected submission",
			},
		})
	}
	// Context delivered while a stop kept this submission, after the model
	// made it, supersedes it when the model can still be asked again.
	since := deliveredSinceCandidate(r.turn, state.callAttempts[call.ID])
	if len(since) > 0 {
		if r.nextCallFits(state, estimatedItemTokens(since)) {
			return r.rejectSubmission(
				call,
				"new context arrived. Read it, then submit again.",
				"delivered context",
				nil,
				false,
			)
		}
		r.deliveryDropped()
	}
	plan := FinishPlan{
		Turn:      snapshotView(state.TurnView),
		AttemptID: state.callAttempts[call.ID],
		CallID:    call.ID,
		Output:    slices.Clone(call.Args),
	}
	if state.latestResult != nil {
		plan.Answer = state.latestResult.Message.Text()
	}
	finish, o := r.finishPoint(state, plan)
	if o != nil {
		return o
	}
	if len(finish.steers) > 0 {
		return r.afterSteerWrite(
			finish.steers,
			// A steer is a nudge, not a veto: the message's text keeps
			// its place in Answer.
			r.rejectSubmission(
				call,
				steerNotAccepted,
				sourceSteering,
				finish.steers,
				false,
			),
		)
	}
	if finish.rejects() {
		if o := r.rejectSubmission(
			call,
			cmp.Or(finish.reason, "a check did not accept this output"),
			"hook "+finish.hook,
			finish.deliver,
			finish.reason != "",
		); o != nil || !finish.noCall {
			return o
		}
		o := r.stop(StopFailed, StopReasonOutputRejected)
		return &o
	}
	out := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("Output accepted.")),
	}
	return r.commit(Step{
		Kind: StepToolSettled,
		Tool: &ToolFact{Call: call, Outcome: &out},
	})
}

// deliveredSinceCandidate returns the context delivered after the model
// attempt that made a candidate answer settled: the same rule the text path
// applies, so a Deliver made before the submission call started counts too.
func deliveredSinceCandidate(t Turn, attemptID string) []ContextItem {
	settled := false
	var items []ContextItem
	for _, step := range t.Steps {
		if (step.Kind == StepModelSettled || step.Kind == StepModelReconciled) &&
			step.Model != nil && step.Model.AttemptID == attemptID {
			settled = true
			continue
		}
		if settled && step.Kind == StepDelivered && step.Context != nil {
			items = append(items, step.Context.Items...)
		}
	}
	return items
}

func boundedOutputError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}

// denialText is the result the model sees for a denied call. An empty
// reason would leave the model guessing why the call did not run.
func denialText(reason string) string {
	if reason == "" {
		return "The call was denied."
	}
	return reason
}
