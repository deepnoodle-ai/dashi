package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/deepnoodle-ai/kernel/tool"
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
	// fatal holds the stop reason of the first result that ends the turn.
	// Once set, no further call starts; running calls still settle.
	fatal := ""
	settle := func(v toolResult) *Outcome {
		if ctx.Err() != nil {
			o := r.contextStop()
			return &o
		}
		step, stop := toolSettlement(v)
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
		select {
		case v := <-results:
			running--
			return settle(v)
		case <-ctx.Done():
			o := r.contextStop()
			return &o
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
			out := tool.Outcome{
				State:  tool.StateNotExecuted,
				Output: new(tool.TextOutput(v.Decision.Reason)),
			}
			if o := r.commit(Step{
				Kind: StepToolSettled,
				Tool: &ToolFact{
					Call:    v.Call,
					Outcome: &out,
					Reason:  v.Decision.Reason,
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
				o := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput("Unknown tool.")),
				}
				if x := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{Call: v.Call, Outcome: &o, Reason: "unknown tool"},
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
			call := v.Call.Clone()
			decision := ToolDecision{}
			var err error
			for changes := range 4 {
				if call.Name == SubmitOutputTool {
					decision = ToolDecision{Kind: DecisionAllow, Revision: "kernel-output-v1"}
					break
				}
				view, _ := Replay(r.turn)
				decision, err = r.b.Policy.AuthorizeTool(ctx, call.Clone(), view)
				if err != nil {
					return r.failureOrContext(err), true
				}
				if ctx.Err() != nil {
					return r.contextStop(), true
				}
				if len(decision.Rewrite) == 0 {
					break
				}
				if !json.Valid(decision.Rewrite) {
					return r.stop(StopFailed, "policy returned invalid tool JSON"), true
				}
				if changes == 3 {
					return r.stop(StopFailed, "tool rewrite limit"), true
				}
				if len(call.RequestedArgs) == 0 {
					call.RequestedArgs = call.Args
				}
				call.Args = append(json.RawMessage(nil), decision.Rewrite...)
				decision.Rewrite = nil
			}
			if decision.Kind != DecisionAllow &&
				decision.Kind != DecisionDeny &&
				decision.Kind != DecisionDefer {
				return r.stop(StopFailed, "invalid tool decision"), true
			}
			if err := validateToolCall(v.Definition, call); err != nil {
				return r.stop(
					StopFailed,
					"policy produced invalid arguments: "+err.Error(),
				), true
			}
			if decision.Kind == DecisionDefer && decision.Revision == "" {
				return r.stop(
					StopFailed,
					"deferred approval requires policy revision",
				), true
			}
			if o := r.commit(Step{
				Kind: StepToolDecision,
				Tool: &ToolFact{
					Call:       call,
					Definition: v.Definition,
					Decision:   decision,
				},
			}); o != nil {
				return *o, true
			}
			v.Call = call
			v.Decision = decision
			if decision.Kind == DecisionDeny {
				out := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput(decision.Reason)),
				}
				if o := r.commit(Step{
					Kind: StepToolSettled,
					Tool: &ToolFact{Call: call, Outcome: &out, Reason: decision.Reason},
				}); o != nil {
					return *o, true
				}
				continue
			}
			if decision.Kind == DecisionDefer {
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
		if v.Definition.Kind == tool.KindExternal {
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
		if r.b.Tools == nil {
			return r.stop(StopFailed, "tool binding unavailable"), true
		}
		bound, err := r.b.Tools.Tools(ctx)
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
		if running >= r.def.Limits.ToolConcurrency {
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
		go func() {
			results <- execute(ctx, ex, call)
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
// panicked, so its result is unknown and the engine records it as uncertain
// rather than running it again.
func execute(
	ctx context.Context,
	ex tool.Executor,
	call tool.Call,
) (result toolResult) {
	result.call = call
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
// result the engine did not observe. An invalid outcome was observed, so it is
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
	out := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("Output accepted.")),
	}
	return r.commit(Step{
		Kind: StepToolSettled,
		Tool: &ToolFact{Call: call, Outcome: &out},
	})
}

func boundedOutputError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}
