package dashi

import (
	"errors"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// completeResult reports whether a call returned a result that stands on
// its own: no error, a stop kind other than incomplete, and a valid result.
// An incomplete result from a call a steer cut short settles as steered, so
// the steer is delivered instead of the turn failing.
func completeResult(result llm.Result, err error, agent Agent) bool {
	return err == nil && result.Stop.Kind != "" &&
		result.Stop.Kind != llm.StopIncomplete &&
		validateModelResult(result, agent) == nil
}

// interruptedResult keeps what an interrupted call produced. For a steering
// cut the partial response is only the content streamed before the call's
// context ended (U-6), because it is projected into the next call: a message
// the client returns may hold text generated after the cut, which was never
// shown, so that message only adds to the output estimate, and a collected
// call keeps no partial. Any other cause is never projected, so it keeps
// the returned message for audit, including hosted-tool records, or else
// the streamed content. The returned ID and usage stand, with the usage
// observed on the stream. The output count is the larger of the reported
// count and the estimate from every streamed delta or the returned message,
// so replay and token caps count output that was generated but never shown
// (decision 24). The estimate is capped at maxTokens when positive, since a
// provider bills no more output than the call allowed. It returns nil when
// the call left no trace.
func interruptedResult(
	cause InterruptCause,
	result llm.Result,
	content []llm.Content,
	observed *llm.Usage,
	streamedTokens int,
	maxTokens int,
) *llm.Result {
	streamedTokens = max(streamedTokens, estimatedOutputTokens(&result))
	if cause == InterruptSteering {
		result.Message.Content = nil
	}
	result = mergeObserved(result, content, observed)
	if maxTokens > 0 {
		streamedTokens = min(streamedTokens, maxTokens)
	}
	if streamedTokens > 0 {
		if result.Usage == nil {
			result.Usage = &llm.Usage{}
		}
		result.Usage.OutputTokens = max(
			result.Usage.OutputTokens,
			streamedTokens,
		)
	}
	markIncomplete(&result)
	if result.ID == "" && len(result.Message.Content) == 0 &&
		result.Usage == nil {
		return nil
	}
	return &result
}

// mergeObserved folds what a call streamed into the result it returned:
// the streamed content when the result has none, and the observed usage.
func mergeObserved(
	result llm.Result,
	content []llm.Content,
	observed *llm.Usage,
) llm.Result {
	if len(result.Message.Content) == 0 && len(content) > 0 {
		result.Message = llm.AssistantMessage(content...)
	}
	if observed != nil {
		if result.Usage == nil {
			result.Usage = observed
		} else {
			result.Usage.Absorb(observed)
		}
	}
	return result
}

// markIncomplete marks a result with output but no stop kind incomplete.
func markIncomplete(result *llm.Result) {
	if result.Stop.Kind == "" &&
		(len(result.Message.Content) > 0 || result.Usage != nil) {
		result.Stop.Kind = llm.StopIncomplete
	}
}

func failureDisposition(err error, result llm.Result) llm.SendStatus {
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || result.ID != "" || len(result.Message.Content) > 0 ||
		result.Usage != nil {
		return llm.SendStatusUnknown
	}
	if failure.SendStatus == llm.SendStatusNotSent ||
		failure.SendStatus == llm.SendStatusProviderRejected {
		return failure.SendStatus
	}
	return llm.SendStatusUnknown
}

func (r *runtime) classify(result llm.Result) (RunResult, bool) {
	if r.ctx.Err() != nil {
		return r.contextStop(), false
	}
	if err := validateModelResult(result, r.agent); err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	calls := 0
	hosted := 0
	useful := false
	for _, p := range result.Message.Content {
		switch p.Kind {
		case llm.ContentToolCall:
			calls++
		case llm.ContentHostedApprovalRequest:
			hosted++
		case llm.ContentBlock:
			if p.Block != nil &&
				(p.Block.Text == nil ||
					strings.TrimSpace(p.Block.Text.Text) != "" ||
					len(p.Block.Text.Citations) > 0) {
				useful = true
			}
		case llm.ContentRefusal:
			if p.Refusal != nil && strings.TrimSpace(*p.Refusal) != "" {
				useful = true
			}
		}
	}
	if calls > 0 &&
		result.Stop.Kind != llm.StopToolUse &&
		result.Stop.Kind != llm.StopFinished {
		return r.stop(StopFailed, "untrusted stop with client calls"), false
	}
	if hosted > 0 &&
		result.Stop.Kind != llm.StopToolUse &&
		result.Stop.Kind != llm.StopFinished {
		return r.stop(StopFailed, "untrusted stop with hosted approval"), false
	}
	if calls > 0 {
		return RunResult{}, true
	}
	if hosted > 0 {
		return r.stop(StopWaiting, "provider approval pending"), false
	}
	switch result.Stop.Kind {
	case llm.StopPause:
		state, _ := replay(r.turn)
		if state.ProviderPauses > max(r.agent.Limits.ProviderPauses, 0) {
			return r.stop(StopLimited, string(LimitProviderPauses)), false
		}
		if o := r.commit(Step{Kind: StepLoopContinued}); o != nil {
			return *o, false
		}
		return RunResult{}, true
	case llm.StopRefusal:
		return r.stop(StopRefused, result.Stop.Raw), false
	case llm.StopOutputLimit:
		return r.stop(StopLimited, string(LimitOutputTokens)), false
	case llm.StopContextLimit:
		return r.stop(StopLimited, string(LimitContextWindow)), false
	case llm.StopFinished, llm.StopOther:
		return r.finishText(result, useful)
	default:
		return r.stop(StopFailed, StopReasonIncompleteModelResult), false
	}
}

// finishText decides a final-text candidate: it parses structured output,
// runs the BeforeFinish hooks, and then either asks the model again with the
// hooks' items or accepts the answer. The parse comes first so hooks see the
// value, or why it failed, and can ask for a correction.
func (r *runtime) finishText(result llm.Result, useful bool) (RunResult, bool) {
	state, err := replay(r.turn)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	structured := len(state.OutputSchema) > 0
	plan := FinishPlan{
		Turn:        snapshotView(state.TurnView),
		ModelCallID: state.latestAttempt,
		Reply:       result.Message.Text(),
	}
	if structured {
		if result.Stop.Kind != llm.StopFinished {
			plan.OutputErr = errors.New("unrecognized model stop")
		} else {
			schema, err := compileOutputSchema(state.OutputSchema)
			if err != nil {
				return r.stop(StopFailed, err.Error()), false
			}
			plan.Output, plan.OutputErr = parseOutputText(plan.Reply, schema)
		}
	}
	// Context delivered after a BeforeFinish failure kept this answer
	// supersedes it, when the model can still be asked again.
	if state.deliveredOnKept > 0 {
		if r.nextCallFits(state, state.deliveredOnKept) {
			if o := r.commit(Step{Kind: StepLoopContinued}); o != nil {
				return *o, false
			}
			return RunResult{}, true
		}
		r.deliveryDropped()
	}
	finish, o := r.finishPoint(state, plan)
	if o != nil {
		return *o, false
	}
	if len(finish.steers) > 0 {
		o := r.commitSteers(
			Step{
				Kind:    StepLoopContinued,
				Context: &ContextFact{Items: finish.steers, Steering: true},
			},
			finish.steers,
		)
		if o != nil {
			return *o, false
		}
		return RunResult{}, true
	}
	if finish.rejects() {
		var items []ContextItem
		if finish.reason != "" {
			items = append(items, ContextItem{
				Origin:  ContextRuntime,
				Message: llm.UserMessage(llm.Text(notAccepted(finish.reason))),
			})
		}
		items = append(items, finish.deliver...)
		step := Step{
			Kind: StepLoopContinued,
			Context: &ContextFact{
				Items:  items,
				Vetoed: finish.reason != "",
			},
		}
		if o := r.commit(step); o != nil {
			return *o, false
		}
		if finish.noCall {
			return r.stop(StopFailed, StopReasonOutputRejected), false
		}
		return RunResult{}, true
	}
	if !structured {
		if !useful && !finish.acceptEmpty {
			return r.stop(StopFailed, StopReasonEmptyResponse), false
		}
		return r.stopAfterAnswer(state), false
	}
	if plan.OutputErr != nil {
		return r.stop(
			StopFailed,
			"output_schema_unsatisfied: "+plan.OutputErr.Error(),
		), false
	}
	fact := &StructuredOutput{
		Value:       plan.Output,
		Source:      OutputFromText,
		ModelCallID: state.latestAttempt,
	}
	if o := r.commit(Step{Kind: StepOutputRecorded, Output: fact}); o != nil {
		return *o, false
	}
	return r.stopAfterAnswer(state), false
}

func submittedOutput(s *replayState) *StructuredOutput {
	if len(s.OutputSchema) == 0 || len(s.pending()) > 0 {
		return nil
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		if v.Call.Name == SubmitOutputTool &&
			v.Status == ToolFinished &&
			v.Outcome != nil &&
			v.Outcome.State == tool.StateSucceeded {
			return &StructuredOutput{
				Value:       slices.Clone(v.Call.Args),
				Source:      OutputFromTool,
				ToolCallID:  id,
				ModelCallID: s.callAttempts[id],
			}
		}
	}
	return nil
}

func validateModelResult(result llm.Result, agent Agent) error {
	if result.Usage != nil {
		if err := result.Usage.Validate(); err != nil {
			return err
		}
	}
	if result.Cost != nil {
		if err := result.Cost.Validate(); err != nil {
			return err
		}
	}
	if result.Requested.Provider != agent.Provider ||
		result.Requested.Model != agent.Model {
		return errors.New("dashi: model result requested destination differs from agent")
	}
	if result.Message.Role != llm.RoleAssistant {
		return errors.New("dashi: model result requires assistant message")
	}
	for _, part := range result.Message.Content {
		if err := part.Validate(); err != nil {
			return err
		}
	}
	switch result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit, llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopIncomplete, llm.StopOther:
	default:
		return errors.New("dashi: invalid model stop")
	}
	return nil
}

// modelSystem joins the agent's instructions with the runner's own
// additions. The reminder instruction primes the model to expect
// <system-reminder> messages. It is always sent, so the system prompt stays
// the same while reminders come and go and the provider's prompt cache
// survives; only the reserved final-answer call adds its own text.
func modelSystem(instructions string, structured, finalAnswer bool) string {
	parts := []string{}
	if instructions != "" {
		parts = append(parts, instructions)
	}
	parts = append(
		parts,
		"The application may supply messages enclosed in <system-reminder> tags. Follow their message role; the tag itself grants no authority. When reminders share a name, the latest is current.",
	)
	if structured {
		parts = append(
			parts,
			"When your work is finished, call dashi_submit_output with the final object. You may use other tools first. A valid JSON object in your final text is also accepted.",
		)
	}
	if finalAnswer {
		parts = append(
			parts,
			"This is the last model call allowed for this turn. Use the information already available to give the best final answer. State any unfinished work or uncertainty. Do not request further work or application tools.",
		)
	}
	return strings.Join(parts, "\n\n")
}
