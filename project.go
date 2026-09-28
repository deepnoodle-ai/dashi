package dashi

import (
	"errors"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Project returns this turn's own model messages without modifying the record.
// It excludes prior conversation history, which the application owns. Unsettled
// client calls and model intents cannot be projected into a new request.
func Project(t Turn) ([]llm.Message, error) {
	return projectWithHistory(t, nil)
}

// modelToolResult is the result the model sees for a settled outcome when
// no result hook replaced it.
func modelToolResult(callID string, o *tool.Outcome) llm.ToolResult {
	output := tool.TextOutput("Tool was not executed.")
	if o.Output != nil {
		output = o.Output.Clone()
	}
	return llm.ToolResult{
		CallID:  callID,
		Output:  output,
		IsError: o.State == tool.StateFailed || o.State == tool.StateNotExecuted,
	}
}

func cloneToolResult(r llm.ToolResult) llm.ToolResult {
	r.Output = r.Output.Clone()
	return r
}

// projectWithHistory assembles one model request from the current call's prior
// history and this turn's recorded messages. It never stores that history.
func projectWithHistory(
	t Turn,
	history []llm.Message,
) ([]llm.Message, error) {
	state, err := replay(t)
	if err != nil {
		return nil, err
	}
	if state.modelIntent != "" || state.unresolvedModel != "" {
		return nil, errors.New("dashi: model intent needs reconciliation")
	}
	for _, id := range state.callOrder {
		if state.Tools[id].Status != ToolSettled {
			return nil, errors.New("dashi: client tool call is unresolved")
		}
	}
	if len(state.hosted) > 0 {
		return nil, errors.New("dashi: hosted approval is unresolved")
	}
	out := make([]llm.Message, 0, len(history)+len(t.Steps))
	for _, m := range history {
		out = append(out, m.Clone())
	}
	type position struct {
		seq   int64
		index int
	}
	latest := map[string]position{}
	settledHosted := map[string]bool{}
	for _, step := range t.Steps {
		if step.Kind == StepHostedApprovalSettled && step.HostedApproval != nil {
			settledHosted[step.HostedApproval.RequestID] = true
		}
		if step.Context != nil {
			for index, item := range step.Context.Items {
				if item.Snapshot {
					latest[contextKey(item)] = position{step.Seq, index}
				}
			}
		}
	}
	// A NextCallOnly item expires once a model call that was dispatched
	// follows it. A request settled without dispatch, or not settled at all,
	// showed the model nothing.
	lastDispatched := lastDispatchedIntent(t)
	for _, step := range t.Steps {
		switch step.Kind {
		case StepStarted:
			for _, m := range step.Started.Input {
				out = append(out, m.Clone())
			}
		case StepDelivered, StepLoopContinued, StepToolSettled:
			// A turned-down submission's items follow its result, which is
			// projected with the model message that made the call.
			if step.Context == nil {
				continue
			}
			for index, item := range step.Context.Items {
				if item.Snapshot && latest[contextKey(item)] != (position{
					step.Seq,
					index,
				}) {
					continue
				}
				if item.NextCallOnly && lastDispatched > step.Seq {
					continue
				}
				out = append(out, item.Message.Clone())
			}
		case StepModelSettled, StepModelReconciled:
			if step.Model.Interrupted == InterruptSteering {
				out = append(out, steeredPartial(step.Model.Result)...)
				continue
			}
			if step.Model.Result == nil || step.Model.Error != "" ||
				step.Model.Interrupted != InterruptNone {
				continue
			}
			m := step.Model.Result.Message.Clone()
			// An incomplete result may contain a partial client call. It was
			// recorded for audit but never admitted as an actionable tool request.
			// Keep its other content without creating a dangling call in history.
			content := make([]llm.Content, 0, len(m.Content))
			for _, part := range m.Content {
				if part.Kind == llm.ContentHostedApprovalRequest {
					if step.Model.Result.Stop.Kind != llm.StopToolUse &&
						step.Model.Result.Stop.Kind != llm.StopFinished {
						continue
					}
					if part.HostedApprovalRequest == nil ||
						!settledHosted[part.HostedApprovalRequest.ID] {
						continue
					}
				}
				if part.Kind == llm.ContentToolCall {
					if step.Model.Result.Stop.Kind != llm.StopToolUse &&
						step.Model.Result.Stop.Kind != llm.StopFinished {
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
				if v.Outcome == nil {
					continue
				}
				result := v.Result()
				out = append(
					out,
					llm.Message{
						Role: llm.RoleUser,
						Content: []llm.Content{
							{
								Kind:       llm.ContentToolResult,
								ToolResult: &result,
							},
						},
					},
				)
			}
		case StepHostedApprovalSettled:
			response := *step.HostedApproval
			out = append(
				out,
				llm.Message{
					Role: llm.RoleUser,
					Content: []llm.Content{
						{
							Kind:                   llm.ContentHostedApprovalResponse,
							HostedApprovalResponse: &response,
						},
					},
				},
			)
		}
	}
	return out, nil
}

// steeredPartial projects a call an urgent steer cut short: its partial
// text as one assistant message, then a reminder that the user interrupted
// it; the steer itself follows in its own step. This refines decision 9,
// which keeps interrupted output from the model, for steering only: the
// person saw that text, and their steer may refer to it. Only text is kept,
// without citations, tool calls, reasoning, or replay data, which a partial
// response cannot carry validly. Trailing whitespace is trimmed and an
// empty partial adds nothing, since providers reject a blank assistant
// message; with no partial, the reminder would describe nothing.
func steeredPartial(result *llm.Result) []llm.Message {
	if result == nil {
		return nil
	}
	text := strings.TrimRightFunc(
		joinText(result.Message.Content),
		unicode.IsSpace,
	)
	if text == "" {
		return nil
	}
	return []llm.Message{
		llm.AssistantMessage(llm.Text(text)),
		steeringReminder(),
	}
}

// steeringReminder tells the model its previous response was cut short. It
// is projected, never recorded, so its wording can change between versions.
func steeringReminder() llm.Message {
	return llm.ReminderMessage(llm.Reminder{
		Name:    "steering-interrupted",
		Tier:    llm.ReminderContextual,
		Content: steeringInterruptedText,
	})
}

// lastDispatchedIntent returns the sequence number of the latest model
// request whose call was dispatched, or zero.
func lastDispatchedIntent(t Turn) int64 {
	intents := map[string]int64{}
	var last int64
	for _, step := range t.Steps {
		switch step.Kind {
		case StepModelIntent:
			intents[step.Model.AttemptID] = step.Seq
		case StepModelSettled, StepModelReconciled:
			if seq := intents[step.Model.AttemptID]; modelDispatched(step.Model) &&
				seq > last {
				last = seq
			}
		}
	}
	return last
}

// modelDispatched reports whether a settlement shows evidence that the call
// reached the provider: any part of a provider response (an ID, content,
// usage, or a stop), or a rejection, which only a provider gives. A
// settlement without such evidence, such as a call stopped before dispatch
// or one recovered after a worker crash, does not count; a recovered call is
// resent on resume, so an item kept for it goes with the resend, and a
// NextCallOnly item may reach the model twice rather than never. A call an
// urgent steer cut short does not count either: its replacement is the
// call the item was meant for.
func modelDispatched(f *ModelFact) bool {
	if f == nil || f.NoDispatch || f.Failure == llm.FailureNotDispatched ||
		f.Interrupted == InterruptSteering {
		return false
	}
	if f.Failure == llm.FailureRejected {
		return true
	}
	r := f.Result
	return r != nil && (r.ID != "" || len(r.Message.Content) > 0 ||
		r.Usage != nil || r.Stop.Kind != "")
}
