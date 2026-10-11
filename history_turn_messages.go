package dashi

import (
	"errors"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/dashi/internal/llmtext"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// TurnMessagesFull returns this turn's own model messages without modifying the record.
// It excludes prior conversation history, which the application owns.
// Unfinished client calls and started model calls cannot go into a new
// request, so TurnMessagesFull fails on a turn that has them, unless the turn
// has ended.
//
// An ended turn (Stop.Ended) always gives its messages, and only up to the
// stop that ended it: results recorded after it count in Spend but never
// change the messages. Each tool call still started, waiting, or with an
// unknown result there shows as an error result, and a model call with no
// recorded result shows nothing.
func TurnMessagesFull(t Turn) ([]llm.Message, error) {
	return publicTurnMessages(t, nil, false)
}

// modelToolResult is the result the model sees for a finished outcome when
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

// sharedToolResult is modelToolResult without the deep copy. The caller
// must only read the result, never change it, and copy any block it keeps.
func sharedToolResult(callID string, o *tool.Outcome) llm.ToolResult {
	output := tool.TextOutput("Tool was not executed.")
	if o.Output != nil {
		output = *o.Output
	}
	return llm.ToolResult{
		CallID:  callID,
		Output:  output,
		IsError: o.State == tool.StateFailed || o.State == tool.StateNotExecuted,
	}
}

// turnMessagesWithHistory assembles one model request from the current
// call's prior history and this turn's recorded messages. It never stores
// that history.
func turnMessagesWithHistory(t Turn, history []llm.Message) ([]llm.Message, error) {
	return publicTurnMessages(t, history, true)
}

// TurnMessages returns this Turn's model context with recorded replacements.
// Source messages remain available through TurnMessagesFull and the immutable facts.
func TurnMessages(t Turn) ([]llm.Message, error) { return publicTurnMessages(t, nil, true) }

func publicTurnMessages(t Turn, history []llm.Message, compact bool) ([]llm.Message, error) {
	messages, err := turnMessages(t, history, compact)
	if err != nil {
		return nil, err
	}
	out := make([]llm.Message, len(messages))
	for i, m := range messages {
		out[i] = m.Message.Clone()
	}
	return out, nil
}

func turnMessages(
	t Turn,
	history []llm.Message,
	compact bool,
) ([]contextMessage, error) {
	return turnMessagesCached(t, history, compact, &contextValidationCache{prefixes: map[int64][]contextMessage{}})
}

func turnMessagesCached(t Turn, history []llm.Message, compact bool, cache *contextValidationCache) ([]contextMessage, error) {
	state, err := replayCached(t, cache)
	if err != nil {
		return nil, err
	}
	return turnMessagesState(t, history, compact, state, cache)
}

func turnMessagesState(t Turn, history []llm.Message, compact bool, state *replayState, cache *contextValidationCache) ([]contextMessage, error) {
	var err error
	ended := state.Stop.Ended
	if ended {
		t.Steps = t.Steps[:endedLength(t.Steps, state.Stop)]
		if state, err = replayCached(t, cache); err != nil {
			return nil, err
		}
	} else {
		if state.modelCallStart != "" || state.unresolvedModel != "" {
			return nil, errors.New("dashi: model call result unknown; the host must resolve it")
		}
		for _, id := range state.callOrder {
			if state.Tools[id].Status != ToolFinished {
				return nil, errors.New("dashi: client tool call is unresolved")
			}
		}
		if len(state.providerTool) > 0 {
			return nil, errors.New("dashi: provider tool approval is unresolved")
		}
	}
	out := make([]contextMessage, 0, len(history)+len(t.Steps))
	var seq int64
	index := 0
	emit := func(messages ...llm.Message) {
		for _, m := range messages {
			out = append(out, contextMessage{Ref: TurnMessageRef{Seq: seq, Index: index}, Message: m})
			index++
		}
	}
	for _, m := range history {
		emit(m.Clone())
	}
	finishedProviderTool := map[string]bool{}
	for _, step := range t.Steps {
		if step.Kind == StepProviderToolApprovalDecided && step.ProviderToolApproval != nil {
			finishedProviderTool[step.ProviderToolApproval.RequestID] = true
		}
	}
	// A NextCallOnly item expires once a model call that was dispatched
	// follows it. A request finished without being sent, or not finished at
	// all, showed the model nothing.
	lastDispatched := lastDispatchedCallStart(t)
	resumed := lastCrashResume(t.Steps)
	emitItems := func(items []ContextItem) {
		// Snapshots append to preserve the request's cached prefix.
		for _, item := range items {
			if item.NextCallOnly && lastDispatched > seq {
				continue
			}
			message := item.Message.Clone()
			message.Context = nil
			if item.Name != "" {
				origin := item.Origin
				if origin == "" {
					origin = ContextUser
				}
				message.Context = &llm.MessageContext{
					Origin: string(origin),
					Name:   item.Name,
				}
			}
			emit(message)
		}
	}
	for _, step := range t.Steps {
		seq, index = step.Seq, 0
		switch step.Kind {
		case StepTurnStarted:
			for _, m := range step.Started.Input {
				emit(m.Clone())
			}
			emitItems(step.Started.Context)
		case StepTurnContinued:
			// The reminder is for the next dispatched model call only, and
			// one is enough however many crashes came before that call. An
			// ended turn makes none.
			if step.Seq == resumed && !ended && lastDispatched <= step.Seq {
				emit(crashResumeReminder())
			}
		case StepMessagesAdded, StepLoopContinued, StepToolFinished:
			// A turned-down submission's items follow its result, which is
			// shown with the model message that made the call.
			if step.Context == nil {
				continue
			}
			emitItems(step.Context.Items)
		case StepModelCallFinished, StepModelCallFinishedByHost:
			if modelPurpose(t, step.Model.ModelCallID) == ModelPurposeCompaction {
				continue
			}
			if step.Model.Interrupted == InterruptSteering {
				emit(steeredPartial(step.Model.Result)...)
				continue
			}
			if step.Model.Result == nil || step.Model.Error != "" ||
				step.Model.Interrupted != InterruptNone {
				continue
			}
			m := step.Model.Result.Message.Clone()
			// An incomplete result may contain a partial client call. It was
			// recorded for audit but never accepted as an actionable tool
			// request. Keep its other content without creating a dangling call
			// in history.
			content := make([]llm.Content, 0, len(m.Content))
			for _, part := range m.Content {
				if part.Kind == llm.ContentProviderToolApprovalRequest {
					if step.Model.Result.Stop.Kind != llm.StopToolUse &&
						step.Model.Result.Stop.Kind != llm.StopFinished {
						continue
					}
					if part.ProviderToolApprovalRequest == nil ||
						!finishedProviderTool[part.ProviderToolApprovalRequest.ID] {
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
					if !ok || v.Status != ToolFinished && !ended {
						continue
					}
				}
				content = append(content, part)
			}
			m.Content = content
			if len(m.Content) == 0 {
				continue
			}
			emit(m)
			for _, part := range m.Content {
				if part.Kind != llm.ContentToolCall || part.ToolCall == nil {
					continue
				}
				v, ok := state.Tools[part.ToolCall.ID]
				if !ok {
					continue
				}
				var result llm.ToolResult
				switch {
				case v.Status != ToolFinished:
					// Only an ended turn shows an unfinished call.
					result = endedToolResult(v.Call.ID)
				case v.Outcome == nil:
					continue
				default:
					result = v.Result()
				}
				emit(llm.Message{
					Role: llm.RoleUser,
					Content: []llm.Content{
						{
							Kind:       llm.ContentToolResult,
							ToolResult: &result,
						},
					},
				})
			}
		case StepContextCompacted:
			if compact {
				out = replaceContext(out, compactionPlan(t, step.Compacted.ModelCallID), *step.Compacted, step.Seq)
			}
		case StepProviderToolApprovalDecided:
			response := *step.ProviderToolApproval
			emit(llm.Message{
				Role: llm.RoleUser,
				Content: []llm.Content{
					{
						Kind:                         llm.ContentProviderToolApprovalResponse,
						ProviderToolApprovalResponse: &response,
					},
				},
			})
		}
	}
	return out, nil
}

// endedLength is how many steps an ended turn's history and reply read:
// all of them, up to the stop that ended it. Nothing but late results follows
// that stop, so the last stop is the one that ended the turn.
func endedLength(steps []Step, stop Stop) int {
	end := len(steps)
	if !stop.Ended {
		return end
	}
	for end > 0 && steps[end-1].Kind != StepTurnStopped {
		end--
	}
	return end
}

// lastCrashResume returns the sequence number of the latest Continue that
// resumed a turn with no recorded stop since the Start or Continue before
// it, whose worker stopped mid-turn; or 0 when there is none.
func lastCrashResume(steps []Step) int64 {
	var last int64
	stopped := false
	for _, step := range steps {
		switch step.Kind {
		case StepTurnStopped:
			stopped = true
		case StepTurnContinued:
			if !stopped {
				last = step.Seq
			}
			stopped = false
		}
	}
	return last
}

// endedToolResult is what the model sees for a call still started, waiting,
// or with an unknown result when its turn ended. Its effect is unknown, so it
// is an error, never a made-up success.
func endedToolResult(callID string) llm.ToolResult {
	return llm.ToolResult{
		CallID:  callID,
		Output:  tool.TextOutput(endedToolText),
		IsError: true,
	}
}

const endedToolText = "Result unknown; the turn ended."

// crashResumeReminder follows a Continue that resumed a turn with no
// recorded stop, whose worker stopped mid-turn. The runner reruns only
// read-only or idempotent calls and marks the rest result unknown; the
// reminder covers the model's side. Like steeringReminder it is added to the
// messages the model sees, never recorded, and a later dispatched call drops
// it.
func crashResumeReminder() llm.Message {
	return llm.ReminderMessage(llm.Reminder{
		Name:    "worker-restarted",
		Tier:    llm.ReminderContextual,
		Content: crashResumeText,
	})
}

const crashResumeText = "The previous worker stopped in the middle of " +
	"this turn. Check the current state before you repeat any action."

// steeredPartial shows a call an urgent steer cut short: its partial
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
		llmtext.Join(result.Message.Content),
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
// is added to the messages the model sees, never recorded, so its wording
// can change between versions.
func steeringReminder() llm.Message {
	return llm.ReminderMessage(llm.Reminder{
		Name:    "steering-interrupted",
		Tier:    llm.ReminderContextual,
		Content: steeringInterruptedText,
	})
}

// lastDispatchedCallStart returns the sequence number of the latest
// model_call_started step whose call was dispatched, or zero.
func lastDispatchedCallStart(t Turn) int64 {
	callStarts := map[string]int64{}
	var last int64
	for _, step := range t.Steps {
		switch step.Kind {
		case StepModelCallStarted:
			callStarts[step.Model.ModelCallID] = step.Seq
		case StepModelCallFinished, StepModelCallFinishedByHost:
			if seq := callStarts[step.Model.ModelCallID]; modelPurpose(t, step.Model.ModelCallID) != ModelPurposeCompaction && modelDispatched(step.Model) &&
				seq > last {
				last = seq
			}
		}
	}
	return last
}

// modelDispatched reports whether a recorded result shows evidence that the
// call reached the provider: any part of a provider response (an ID, content,
// usage, or a stop), or a rejection, which only a provider gives. A
// recorded result without such evidence, such as a call stopped before dispatch
// or one recovered after a worker crash, does not count; a recovered call is
// resent on resume, so an item kept for it goes with the resend, and a
// NextCallOnly item may reach the model twice rather than never. A call an
// urgent steer cut short does not count either: its replacement is the
// call the item was meant for.
func modelDispatched(f *ModelFact) bool {
	if f == nil || f.NotSent || f.SendStatus == llm.SendStatusNotSent ||
		f.Interrupted == InterruptSteering {
		return false
	}
	if f.SendStatus == llm.SendStatusProviderRejected {
		return true
	}
	r := f.Result
	return r != nil && (r.ID != "" || len(r.Message.Content) > 0 ||
		r.Usage != nil || r.Stop.Kind != "")
}
