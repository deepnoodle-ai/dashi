package kernel

import (
	"context"
	"encoding/json"

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
	drainOne := func() *Outcome { v := <-results; running--; return settle(v) }
	for _, id := range s.callOrder {
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
			if err := validateToolCall(v.Definition, v.Call); err != nil {
				out := tool.Outcome{State: tool.StateNotExecuted, Output: ptr(tool.TextOutput("Invalid tool arguments: " + err.Error()))}
				if x := r.commit(Step{Kind: StepToolSettled, Tool: &ToolFact{Call: v.Call, Outcome: &out, Reason: "invalid arguments"}}); x != nil {
					return *x, true
				}
				continue
			}
			call := v.Call.Clone()
			decision := ToolDecision{}
			var err error
			for changes := 0; changes < 4; changes++ {
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
		if o := r.commit(Step{Kind: StepToolStarted, Tool: &ToolFact{Call: v.Call, Definition: v.Definition}}); o != nil {
			cancel()
			return *o, true
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
