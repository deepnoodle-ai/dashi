package kernel

import (
	"errors"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// Project builds the next model-facing history without modifying the record.
// The caller supplies earlier session history if desired. Unsettled client
// calls and model intents cannot be projected into a new request.
func Project(history []llm.Message, t Turn) ([]llm.Message, error) {
	state, err := replay(t)
	if err != nil {
		return nil, err
	}
	if state.modelIntent != "" || state.unresolvedModel != "" {
		return nil, errors.New("kernel: model intent needs reconciliation")
	}
	for _, id := range state.callOrder {
		if state.Tools[id].Status != ToolSettled {
			return nil, errors.New("kernel: client tool call is unresolved")
		}
	}
	if len(state.hosted) > 0 {
		return nil, errors.New("kernel: hosted approval is unresolved")
	}
	out := make([]llm.Message, 0, len(history)+len(t.Steps))
	for _, m := range history {
		out = append(out, m.Clone())
	}
	latest := map[string]int64{}
	for _, step := range t.Steps {
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if item.Snapshot {
					latest[item.Origin+"\x00"+item.Name] = step.Seq
				}
			}
		}
	}
	var lastIntent int64
	for _, step := range t.Steps {
		if step.Kind == StepModelIntent {
			lastIntent = step.Seq
		}
	}
	for _, step := range t.Steps {
		switch step.Kind {
		case StepStarted:
			for _, m := range step.Started.Input {
				out = append(out, m.Clone())
			}
		case StepDelivered, StepLoopContinued:
			if step.Context == nil {
				continue
			}
			for _, item := range step.Context.Items {
				if item.Snapshot && latest[item.Origin+"\x00"+item.Name] != step.Seq {
					continue
				}
				if item.NextCallOnly && lastIntent > step.Seq {
					continue
				}
				out = append(out, item.Message.Clone())
			}
		case StepModelSettled, StepModelReconciled:
			if step.Model.Result == nil || step.Model.Error != "" {
				continue
			}
			m := step.Model.Result.Message.Clone()
			// An incomplete result may contain a partial client call. It was
			// recorded for audit but never admitted as an actionable tool request.
			// Keep its other content without creating a dangling call in history.
			content := make([]llm.Content, 0, len(m.Content))
			for _, part := range m.Content {
				if part.Kind == llm.ContentHostedApprovalRequest && step.Model.Result.Stop.Kind != llm.StopToolUse && step.Model.Result.Stop.Kind != llm.StopFinished {
					continue
				}
				if part.Kind == llm.ContentToolCall {
					if step.Model.Result.Stop.Kind != llm.StopToolUse && step.Model.Result.Stop.Kind != llm.StopFinished {
						continue
					}
					if part.ToolCall == nil {
						continue
					}
					v, ok := state.Tools[part.ToolCall.ID]
					if !ok || v.Status != ToolSettled {
						continue
					}
				}
				content = append(content, part)
			}
			m.Content = content
			if len(m.Content) == 0 {
				continue
			}
			out = append(out, m)
			for _, part := range m.Content {
				if part.Kind != llm.ContentToolCall || part.ToolCall == nil {
					continue
				}
				v, ok := state.Tools[part.ToolCall.ID]
				if !ok {
					continue
				}
				o := v.Outcome
				if o == nil {
					continue
				}
				output := tool.Output{}
				if o.Output != nil {
					output = o.Output.Clone()
				} else {
					output = tool.TextOutput("Tool was not executed.")
				}
				bad := o.State == tool.StateFailed || o.State == tool.StateNotExecuted
				out = append(out, llm.Message{Role: llm.RoleUser, Content: []llm.Content{{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: v.Call.ID, Output: output, IsError: bad}}}})
			}
		case StepHostedApproval:
			response := *step.HostedApproval
			out = append(out, llm.Message{Role: llm.RoleUser, Content: []llm.Content{{Kind: llm.ContentHostedApprovalResponse, HostedApprovalResponse: &response}}})
		}
	}
	return out, nil
}
