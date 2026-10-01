package dashi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/tooldef"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (r *runtime) model(s *replayState) (Outcome, bool) { return r.modelPurpose(s, "", nil) }

func (r *runtime) modelPurpose(s *replayState, purpose ModelPurpose, plan *ContextCompactionPlan) (Outcome, bool) {
	// Steers waiting now reach this call. They are recorded before the
	// BeforeModel hooks run, so the hooks' input includes them.
	if steers := r.waitingSteers(s); len(steers) > 0 {
		o := r.commitSteers(
			Step{
				Kind:    StepDelivered,
				Context: &ContextFact{Items: steers, Steering: true},
			},
			steers,
		)
		if o != nil {
			return *o, false
		}
		var err error
		if s, err = replay(r.turn); err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	finalAnswerOnly := purpose == "" && !r.agent.Limits.NoReservedFinalAnswer &&
		s.ModelAttempts > 0 &&
		s.ModelAttempts+1 == s.attemptCap
	messages, err := projectWithHistory(r.turn, r.history)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	defs := []tool.Definition{}
	if purpose == "" && !finalAnswerOnly && !r.agent.NoTools {
		// Without a toolset the selection still runs, so a named tool
		// warns that it is not offered.
		var tools []tool.Tool
		var e error
		if r.runner.Tools != nil {
			tools, e = r.runner.Tools.Tools(r.toolsContext(r.ctx))
			if e != nil {
				return r.failureOrContext(e), false
			}
		}
		tools = r.selectTools(s, tools)
		for _, t := range tools {
			if e = t.Definition.Validate(); e != nil {
				return r.stop(StopFailed, e.Error()), false
			}
			if t.Definition.Name == SubmitOutputTool {
				return r.stop(
					StopFailed,
					"tool name is reserved for structured output",
				), false
			}
			defs = append(defs, t.Definition.Clone())
		}
	}
	structured := purpose == "" && len(s.OutputSchema) > 0
	if structured {
		defs = append(
			defs,
			tool.Definition{
				Name:        SubmitOutputTool,
				Description: "Submit the final JSON object when all other work is finished. Its arguments must satisfy the output schema.",
				Schema:      slices.Clone(s.OutputSchema),
				Kind:        tool.KindLocal,
				Annotations: tool.Annotations{ReadOnly: new(true), Idempotent: new(true)},
			},
		)
	}
	system := modelSystem(r.agent.Instructions, structured, finalAnswerOnly)
	input := llm.Input{
		Model:    r.agent.Model,
		System:   system,
		Messages: messages,
		Tools:    defs,
		Settings: r.agent.Settings,
		Stream:   r.opts.stream,
	}
	if purpose == ModelPurposeCompaction {
		input.System = activeSummaryPrompt
		input.Messages, err = r.activeSummaryMessages(plan)
		if err != nil {
			return r.stop(StopFailed, StopReasonCompactionFailed), false
		}
		input.Settings.MaxTokens = r.summaryMaxTokens(input.Settings.MaxTokens)
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	if key := r.runner.CacheKey; key != "" &&
		input.Settings.PromptCache.Key == "" {
		info, _ := r.runner.Client.ModelInfo(input.Model)
		if info.Capabilities.CacheKeyDefault {
			input.Settings.PromptCache.Key = key
		}
	}
	if finalAnswerOnly {
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	// A request abandoned for an urgent steer before dispatch drops the
	// hook answers of this pass; the next pass asks the hooks again.
	mark := r.hookMark()
	deliver, hidden, hookStop, ctxStop := r.beforeModel(s, &input, finalAnswerOnly, purpose)
	// The intent records the tools offered, after any hides, and the names
	// hidden, so a call to a hidden tool resolves from the record.
	defs = input.Tools
	if ctxStop != nil {
		return *ctxStop, false
	}
	if hookStop != nil {
		return r.stopWith(hookStop), false
	}
	if len(deliver) > 0 {
		if o := r.commit(Step{
			Kind:    StepDelivered,
			Context: &ContextFact{Items: deliver},
		}); o != nil {
			return *o, false
		}
		mark = r.hookMark()
		if purpose == ModelPurposeCompaction {
			input.Messages, err = r.activeSummaryMessages(plan)
		} else {
			input.Messages, err = projectWithHistory(r.turn, r.history)
		}
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	if purpose == ModelPurposeCompaction {
		input.Settings.MaxTokens = r.summaryMaxTokens(input.Settings.MaxTokens)
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
		input.Tools = nil
		defs = nil
	}
	if finalAnswerOnly {
		// The turn-wide call limit wins over a forced application-tool choice.
		// BeforeSend still authorizes the actual prepared request.
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	if purpose == "" && len(s.OutputSchema) > 0 {
		if err := validateOutputToolChoice(input.Settings); err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	// Native model-call output is direct-call-only until the turn's recorded
	// output and recovery path can use it. Keep this guard after hooks so a
	// future hook shape cannot activate it without a turn design.
	if len(input.OutputSchema) > 0 {
		return r.stop(StopFailed, "native output_schema is not supported in agent turns"), false
	}
	var budgetWarnings []Warning
	if stop := incompleteUsageStop(s); stop != nil {
		return r.stopWith(stop), false
	}
	if s.Budget != nil && s.Budget.MaxOutputTokens > 0 {
		remaining := s.Budget.MaxOutputTokens - s.Spend.Usage.OutputTokens
		if remaining <= 0 {
			return r.stopWith(budgetSpentStop(s)), false
		}
		if input.Settings.MaxTokens == 0 || input.Settings.MaxTokens > remaining {
			old := input.Settings.MaxTokens
			input.Settings.MaxTokens = remaining
			budgetWarnings = append(
				budgetWarnings,
				Warning{
					Code: WarningBudgetClamp,
					Message: fmt.Sprintf(
						"max_tokens %d bounded to %d by the turn output-token budget",
						old,
						remaining,
					),
				},
			)
		}
	}
	// References become verified bytes only here, after every hook has seen
	// the recorded input and before it is prepared and sent.
	input, refStop := r.hydrate(input)
	if refStop != nil {
		return *refStop, false
	}
	req, err := r.runner.Client.Prepare(r.ctx, input)
	if err != nil {
		return r.failureOrContext(err), false
	}
	if origin := req.Origin(); origin.Provider != r.agent.Provider ||
		origin.API != r.agent.API ||
		origin.Model != r.agent.Model {
		return r.stop(StopFailed, "prepared request changed destination"), false
	}
	snapshot := req.Snapshot()
	if !canonjson.SameValue(snapshot.Input, input) {
		return r.stop(
			StopFailed,
			"prepared request changed original model input",
		), false
	}
	if r.activeMediaEstimateUnavailable(s, snapshot.Input) {
		// Content preparation and provider acceptance govern this candidate.
		// Financial admission still uses the unchanged conservative risk estimate.
	} else if purpose == "" {
		if o, again, handled := r.guardActiveContext(s, input, req); handled {
			return o, again
		}
	} else if llm.EstimatedInputTokens(req) > max(0, s.ActiveCompactionWindow-req.Snapshot().Effective.MaxTokens) {
		return r.contextLimit(s, req, "summary evidence or hook context does not fit"), false
	}
	// The call runs under its own context, so an urgent steer can end it
	// without ending the run (U-6).
	callCtx, cancelCall := context.WithCancelCause(r.ctx)
	defer cancelCall(nil)
	never := ""
	switch {
	case tooldef.UnsafeHosted(defs):
		never = "it offers a provider-hosted tool that is not safe to repeat"
	case s.pausedHosted:
		// The steer could not follow the paused result anyway (FR-6).
		never = "it finishes a hosted tool call the provider paused"
	}
	call, back := r.registerCall(cancelCall, never)
	defer r.dropCall(call)
	if back {
		r.dropHooks(mark)
		return Outcome{}, true
	}
	atRisk, remainingCost, costStop := checkSendBudget(s, req, r.agent.Model)
	if costStop != nil {
		return r.stopWith(costStop), false
	}
	admission, sendStop, ctxStop := r.beforeSend(s, req, atRisk, remainingCost, purpose)
	if ctxStop != nil {
		return *ctxStop, false
	}
	if sendStop != nil {
		return r.stopWith(sendStop), false
	}
	if r.ctx.Err() != nil {
		return r.contextStop(), false
	}
	if r.pauseRequested() {
		return r.pauseStop(), false
	}
	if r.wasCut(call) {
		// Nothing was recorded or sent: go back for the steer.
		r.dropHooks(mark)
		return Outcome{}, true
	}
	attempt := fmt.Sprintf("%s:model:%d", r.turn.ID, s.modelCalls+1)
	messageID := attempt + ":message"
	fact := &ModelFact{
		Purpose:              purpose,
		Compaction:           plan.clone(),
		AttemptID:            attempt,
		MessageID:            messageID,
		RequestDigest:        canonjson.Digest(req.Body()),
		FinalAnswerOnly:      finalAnswerOnly,
		Admission:            admission,
		AtRisk:               atRisk,
		EstimatedInputTokens: llm.EstimatedInputTokens(req),
		Provider:             r.runner.Client.Provider(),
		API:                  r.runner.Client.API(),
		Model:                r.agent.Model,
		Effective:            snapshot.Effective,
		Origin:               snapshot.Origin,
		Adjustments:          snapshot.Adjustments,
		ContentChanges:       snapshot.ContentChanges,
		ModelInfo:            snapshot.ModelInfo,
		CapabilityPolicy:     snapshot.CapabilityPolicy,
		Unverified:           snapshot.Unverified,
		Tools:                defs,
		Hidden:               hidden,
	}
	if o := r.commit(Step{
		Kind:     StepModelIntent,
		Model:    fact,
		Warnings: budgetWarnings,
	}); o != nil {
		return *o, false
	}
	if err := r.ctx.Err(); err != nil || r.pauseRequested() {
		reason := "pause requested"
		if err != nil {
			reason = context.Cause(r.ctx).Error()
		}
		settled := &ModelFact{
			AttemptID:   attempt,
			MessageID:   messageID,
			Error:       reason,
			Interrupted: InterruptRun,
			NoDispatch:  true,
		}
		if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
			return *o, false
		}
		if err != nil {
			return r.contextStop(), false
		}
		return r.pauseStop(), false
	}
	if h := r.handOff(call, attempt, messageID); h.done {
		return h.outcome, h.again
	}
	var result llm.Result
	var sendErr error
	var observedContent []llm.Content
	lastTextIndex := -1
	var observedUsage *llm.Usage
	var streamed streamedOutput
	for item, err := range llm.Stream(
		callCtx,
		r.runner.Client,
		llm.Call{
			Request: req,
			// The client may key durable work by this ID; see llm.Attempt.
			Attempt: llm.Attempt{TurnID: r.turn.ID, AttemptID: attempt},
		},
	) {
		if item.Delta != nil {
			if item.Delta.Usage != nil {
				if observedUsage == nil {
					observedUsage = &llm.Usage{}
				}
				observedUsage.Absorb(item.Delta.Usage)
			}
			streamed.add(*item.Delta)
			// Output that arrives after the call was cut short was never
			// shown, so it is not part of the partial response (U-6).
			live := callCtx.Err() == nil
			if live && item.Delta.Content != nil {
				observedContent = append(
					observedContent,
					item.Delta.Content.Clone(),
				)
				lastTextIndex = item.Delta.Index
			} else if live && item.Delta.Kind == llm.DeltaText &&
				item.Delta.Text != "" {
				last := len(observedContent) - 1
				if last >= 0 && lastTextIndex == item.Delta.Index &&
					observedContent[last].Block != nil &&
					observedContent[last].Block.Text != nil {
					observedContent[last].Block.Text.Text += item.Delta.Text
				} else {
					observedContent = append(
						observedContent,
						llm.Text(item.Delta.Text),
					)
				}
				lastTextIndex = item.Delta.Index
			}
			if live {
				r.emit(Event{
					TurnID:    r.turn.ID,
					AttemptID: attempt,
					MessageID: messageID,
					Delta:     item.Delta,
				})
			}
		}
		if item.Result != nil {
			result = *item.Result
			sendErr = err
		}
	}
	r.unregisterCall(call)
	// Every interrupt, whatever its cause, folds the estimate from all
	// streamed deltas into the recorded output (U-9), so text that arrived
	// after the cut still counts.
	partial := func(cause InterruptCause) *llm.Result {
		return interruptedResult(
			cause,
			result,
			observedContent,
			observedUsage,
			streamed.tokens(),
			fact.Effective.MaxTokens,
		)
	}
	var sendReason llm.ErrorReason
	if failure, ok := errors.AsType[*llm.Error](sendErr); ok {
		sendReason = failure.Reason
	}
	timeoutWon := sendReason == llm.ReasonResponseStartTimeout ||
		sendReason == llm.ReasonStreamIdleTimeout ||
		sendReason == llm.ReasonCallTimeout
	settled := &ModelFact{AttemptID: attempt, MessageID: messageID}
	// A Dashi timeout reason proves its own timer fired before the cut, so
	// the timeout wins and the steer stays undelivered. A result that
	// completed as the cancel arrived also stands, and the steer waits for
	// the next call (U-7).
	if !timeoutWon && r.ctx.Err() == nil &&
		errors.Is(context.Cause(callCtx), errSteered) &&
		!completeResult(result, sendErr, r.agent) {
		return r.settleSteered(
			s,
			fact,
			settled,
			partial(InterruptSteering),
			sendErr,
			result,
		)
	}
	if r.ctx.Err() != nil && !timeoutWon {
		settled.Interrupted = InterruptRun
		settled.Error = context.Cause(r.ctx).Error()
		settled.Uncertain = tooldef.UnsafeHosted(defs)
		settled.Result = partial(InterruptRun)
		setSettlementCharge(fact, settled)
		step := Step{Kind: StepModelSettled, Model: settled}
		if o := r.commit(step); o != nil {
			return *o, false
		}
		if settled.Uncertain && r.runner.OnContextDone != ContextCancel &&
			!errors.Is(context.Cause(r.ctx), errTurnDeadline) {
			return r.stop(
				StopUncertain,
				"provider-hosted tool may have run",
			), false
		}
		return r.contextStop(), false
	}
	var validationErr error
	if sendErr != nil {
		// A failed call keeps what it streamed, with its reported usage.
		result = mergeObserved(result, observedContent, observedUsage)
		markIncomplete(&result)
		settled.Error = sendErr.Error()
		settled.ErrorReason = sendReason
		settled.Failure = failureDisposition(sendErr, result)
		settled.Uncertain = settled.Failure == llm.FailureUnknown
		if !settled.Uncertain {
			if failure, ok := errors.AsType[*llm.Error](sendErr); ok {
				settled.Evidence = fmt.Sprintf(
					"client disposition %s; HTTP %d",
					failure.Disposition,
					failure.Status,
				)
			}
		}
	} else {
		if purpose == ModelPurposeCompaction {
			validationErr = validateModelResult(result, r.agent)
			if validationErr == nil {
				_, validationErr = validActiveSummary(&result)
			}
			if validationErr == nil {
				_, validationErr = r.activeEstimateAfter(plan, &result)
			}
		} else {
			validationErr = validateModelResult(result, r.agent)
		}
		if validationErr != nil {
			settled.Error = validationErr.Error()
		}
	}
	if sendErr == nil ||
		result.Stop.Kind != "" ||
		result.ID != "" ||
		len(result.Message.Content) > 0 ||
		result.Usage != nil {
		settled.Result = &result
	}
	setSettlementCharge(fact, settled)
	if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
		return *o, false
	}
	if r.ctx.Err() != nil && !timeoutWon {
		return r.contextStop(), false
	}
	if sendErr != nil {
		if settled.Uncertain {
			return r.stop(StopUncertain, sendErr.Error()), false
		}
		return r.stop(StopFailed, sendErr.Error()), false
	}
	if validationErr != nil {
		if purpose == ModelPurposeCompaction {
			return r.stop(StopFailed, StopReasonCompactionFailed), false
		}
		return r.stop(StopFailed, validationErr.Error()), false
	}
	current, replayErr := replay(r.turn)
	if replayErr != nil {
		return r.stop(StopFailed, replayErr.Error()), false
	}
	if stop := budgetOvershotStop(current); stop != nil {
		return r.stopWith(stop), false
	}
	if purpose == ModelPurposeCompaction {
		return Outcome{}, true
	}
	r.compactionUsed = false
	r.compactionCandidate = nil
	return r.classify(result)
}

// handOffResult says what the run does after handOff. When done is false
// the call was handed off and the run streams it. Otherwise the run returns
// outcome, going back for the steer when again is set.
type handOffResult struct {
	done    bool
	outcome Outcome
	again   bool
}

// handOff marks call handed to the client, or, when an urgent steer cut it
// short after its request was recorded but before the hand-off, settles it
// as not dispatched, so the run goes back for the steer.
func (r *runtime) handOff(
	call *steerableCall,
	attempt string,
	messageID string,
) handOffResult {
	if call == nil {
		return handOffResult{}
	}
	// The listener gets its own replay of the turn, taken here with the
	// intent recorded, because it runs on another goroutine: sharing the
	// run's replay state would race with the run's commits.
	inFlight, err := replay(r.turn)
	if err != nil {
		return handOffResult{
			done:    true,
			outcome: r.stop(StopFailed, err.Error()),
		}
	}
	fits := func(extraInputTokens int) string {
		if r.pauseRequested() {
			return "a pause was requested"
		}
		return r.callBlocked(inFlight, extraInputTokens)
	}
	if !r.cutBeforeHandOff(call, fits) {
		return handOffResult{}
	}
	r.unregisterCall(call)
	settled := &ModelFact{
		AttemptID:   attempt,
		MessageID:   messageID,
		Error:       errSteered.Error(),
		Interrupted: InterruptSteering,
		NoDispatch:  true,
	}
	if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
		return handOffResult{done: true, outcome: *o}
	}
	return handOffResult{done: true, again: true}
}

// settleSteered records a call an urgent steer cut short after the hand-off
// and goes back for the steer. The call counts as sent unless the client
// reports it was not dispatched: a missing response proves nothing. It
// does not count toward ModelAttempts (U-8). Under a cost cap it is charged
// the worst case reserved at admission, since a client may keep generating
// after the cancel and the usage it reports so far is not final (U-9).
func (r *runtime) settleSteered(
	s *replayState,
	intent *ModelFact,
	settled *ModelFact,
	partial *llm.Result,
	sendErr error,
	result llm.Result,
) (Outcome, bool) {
	settled.Interrupted = InterruptSteering
	settled.Error = errSteered.Error()
	// A FailureRejected error that coincides with the cut settles as sent,
	// and under a cap is charged the worst case. That over-charges a call
	// the provider refused, which is harmless and keeps the rule simple.
	switch {
	case failureDisposition(sendErr, result) == llm.FailureNotDispatched:
		settled.NoDispatch = true
	case s.Budget != nil && s.Budget.MaxCost != nil && intent.AtRisk != nil:
		settled.Result = partial
		charge := intent.AtRisk.Clone()
		settled.Charge = &charge
		settled.EstimatedUsage = true
	default:
		settled.Result = partial
		setSettlementCharge(intent, settled)
	}
	if o := r.commit(Step{Kind: StepModelSettled, Model: settled}); o != nil {
		return *o, false
	}
	return Outcome{}, true
}

// streamedOutput sizes every generated delta of a call, including those
// that arrive after it was cut short: text, reasoning, and tool arguments.
type streamedOutput struct {
	runes int
}

func (o *streamedOutput) add(delta llm.Delta) {
	switch delta.Kind {
	case llm.DeltaText, llm.DeltaReasoning, llm.DeltaToolArguments:
		o.runes += utf8.RuneCountInString(delta.Text)
	}
}

// tokens estimates output tokens as estimatedOutputTokens does.
func (o streamedOutput) tokens() int {
	return o.runes/4 + min(1, o.runes%4)
}
