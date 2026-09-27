package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/kernel/tool"
)

type toolResult struct {
	call    tool.Call
	outcome tool.Outcome
	err     error
}

func (r *runtime) tools(s *replayState) (Outcome, bool) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	results := make(chan toolResult, len(s.callOrder))
	running := 0
	settle := func(v toolResult) *Outcome {
		fact := &ToolFact{Call: v.call}
		if v.err != nil {
			fact.Reason = v.err.Error()
		} else {
			fact.Outcome = &v.outcome
		}
		kind := StepToolSettled
		if v.err == nil && (v.outcome.State == tool.StateWaiting || v.outcome.State == tool.StateDetached) {
			kind = StepToolWaiting
		}
		return r.commit(Step{Kind: kind, Tool: fact})
	}
	drainOne := func() *Outcome {
		select {
		case v := <-results:
			running--
			return settle(v)
		case <-ctx.Done():
			o := r.stop(StopCanceled, ctx.Err().Error())
			return &o
		}
	}
	for _, id := range s.callOrder {
		if err := ctx.Err(); err != nil {
			return r.stop(StopCanceled, err.Error()), true
		}
		state, _ := replay(r.turn)
		v := state.Tools[id]
		if v.Status == ToolSettled || v.Status == ToolWaiting || v.Status == ToolApprovalPending || v.Status == ToolUncertain {
			continue
		}
		if v.Status == ToolDenied {
			out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput(v.Decision.Reason))}
			if o := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: v.Decision.Reason}}); o != nil {
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
			if o := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Reason: "previous execution may have taken effect"}}); o != nil {
				return *o, true
			}
			continue
		}
		if v.Status == ToolRequested {
			if v.Definition.Name == "" {
				o := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput("Unknown tool."))}
				if x := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &o, Reason: "unknown tool"}}); x != nil {
					return *x, true
				}
				continue
			}
			if v.Call.Name == SubmitOutputTool && len(v.Call.Args) > maxOutputValueBytes {
				out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput("Output rejected because it is too large. Return a smaller object and submit again."))}
				if x := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: "output exceeds size limit"}}); x != nil {
					return *x, true
				}
				continue
			}
			if err := validateToolCall(v.Definition, v.Call); err != nil {
				message := "Invalid tool arguments: " + err.Error()
				if v.Call.Name == SubmitOutputTool {
					message = "Output rejected: " + boundedOutputError(err) + ". Correct it and call " + SubmitOutputTool + " again."
				}
				out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput(message))}
				if x := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: "invalid arguments"}}); x != nil {
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
					out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput("Submit output in a separate model response after other tool calls finish."))}
					if x := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: "submission shared a tool batch"}}); x != nil {
						return *x, true
					}
					continue
				}
			}
			call := v.Call.Clone()
			decision := ToolDecision{}
			var err error
			for changes := 0; changes < 4; changes++ {
				if call.Name == SubmitOutputTool {
					decision = ToolDecision{Kind: DecisionAllow, Revision: "kernel-output-v1"}
					break
				}
				view, _ := Replay(r.turn)
				decision, err = r.b.Policy.AuthorizeTool(ctx, call.Clone(), view)
				if err != nil {
					return r.stop(StopFailed, err.Error()), true
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
			if decision.Kind != DecisionAllow && decision.Kind != DecisionDeny && decision.Kind != DecisionDefer {
				return r.stop(StopFailed, "invalid tool decision"), true
			}
			if err := validateToolCall(v.Definition, call); err != nil {
				return r.stop(StopFailed, "policy produced invalid arguments: "+err.Error()), true
			}
			if decision.Kind == DecisionDefer && decision.Revision == "" {
				return r.stop(StopFailed, "deferred approval requires policy revision"), true
			}
			if o := r.commit(Step{Kind: StepToolDecision, Tool: &ToolFact{Call: call, Definition: v.Definition, Decision: decision}}); o != nil {
				return *o, true
			}
			v.Call = call
			v.Decision = decision
			if decision.Kind == DecisionDeny {
				out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput(decision.Reason))}
				if o := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: call, Outcome: &out, Reason: decision.Reason}}); o != nil {
					return *o, true
				}
				continue
			}
			if decision.Kind == DecisionDefer {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return r.stop(StopCanceled, err.Error()), true
		}
		if v.Call.Name == SubmitOutputTool {
			if o := r.commit(Step{Kind: StepToolStarted, Tool: &ToolFact{Call: v.Call, Definition: v.Definition}}); o != nil {
				return *o, true
			}
			if o := r.settleOutputSubmission(v.Call); o != nil {
				return *o, true
			}
			continue
		}
		if v.Definition.Kind == tool.KindExternal {
			wait := tool.Outcome{State: tool.StateWaiting, Wait: &tool.Wait{Reason: "external execution"}}
			if o := r.commit(Step{Kind: StepToolWaiting, Tool: &ToolFact{Call: v.Call, Outcome: &wait}}); o != nil {
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
			return r.stop(StopFailed, err.Error()), true
		}
		var ex tool.Executor
		for _, candidate := range bound {
			if candidate.Definition.Name == v.Call.Name && digest(candidate.Definition) == digest(v.Definition) {
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
		}
		if err := ctx.Err(); err != nil {
			return r.stop(StopCanceled, err.Error()), true
		}
		if o := r.commit(Step{Kind: StepToolStarted, Tool: &ToolFact{Call: v.Call, Definition: v.Definition}}); o != nil {
			cancel()
			return *o, true
		}
		if err := ctx.Err(); err != nil {
			out := tool.Outcome{State: tool.StateNotExecuted}
			if o := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: err.Error()}}); o != nil {
				return *o, true
			}
			return r.stop(StopCanceled, err.Error()), true
		}
		running++
		call := v.Call.Clone()
		go func() {
			out, err := ex.Execute(ctx, call)
			if err == nil {
				if e := out.Validate(); e != nil {
					err = e
				}
			}
			results <- toolResult{call: call, outcome: out, err: err}
		}()
	}
	for running > 0 {
		if o := drainOne(); o != nil {
			cancel()
			return *o, true
		}
	}
	state, _ := replay(r.turn)
	for _, id := range state.callOrder {
		v := state.Tools[id]
		if v.Status == ToolUncertain {
			return r.stop(StopUncertain, "tool outcome needs reconciliation"), true
		}
	}
	if hasOpenTools(state) {
		return r.stop(StopWaiting, "tool result or approval pending"), true
	}
	return Outcome{}, false
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
		message := fmt.Sprintf("Output rejected: %s. Correct it and call %s again.", boundedOutputError(err), SubmitOutputTool)
		out := tool.Outcome{State: tool.StateFailed, Output: ptr(tool.TextOutput(message))}
		return r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: call, Outcome: &out, Reason: "output schema rejected submission"}})
	}
	out := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("Output accepted."))}
	return r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: call, Outcome: &out}})
}

func boundedOutputError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}
