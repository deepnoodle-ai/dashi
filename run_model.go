package dashi

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/internal/tooldef"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (r *runtime) model(s *replayState) (RunResult, bool) { return r.modelPurpose(s, "", nil) }

func (r *runtime) modelPurpose(s *replayState, purpose ModelPurpose, plan *ContextCompactionPlan) (RunResult, bool) {
	// Runner.apply rejects a run that could get here without a Client
	// before it records anything; this guards the invariant.
	if r.runner.Client == nil {
		return r.stop(StopFailed, errNoModelClient.Error()), false
	}
	// Steers waiting now reach this call. They are recorded before the
	// BeforeModel hooks run, so the hooks' input includes them. A summary
	// call takes none: they wait for the ordinary call after it.
	var steers []ContextItem
	if purpose != ModelPurposeCompaction {
		steers = r.waitingSteers(s)
	}
	if len(steers) > 0 {
		o := r.commitSteers(
			Step{
				Kind:    StepMessagesAdded,
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
	finalReplyOnly := purpose == "" && !r.agent.Limits.NoReservedFinalReply &&
		s.ModelCalls > 0 &&
		s.ModelCalls+1 == s.modelCallCap
	messages, err := turnMessagesWithHistory(r.turn, r.history)
	if err != nil {
		return r.stop(StopFailed, err.Error()), false
	}
	defs := []tool.Definition{}
	if purpose == "" && !finalReplyOnly && !r.agent.NoTools {
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
	system := modelSystem(r.agent.Instructions, structured, finalReplyOnly)
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
		// A summary is not work toward the Turn's answer, so it gets no
		// task budget.
		input.Settings.TaskBudget = nil
	} else {
		input.Settings.TaskBudget, err = turnTaskBudget(r.turn, input.Settings.TaskBudget)
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	if key := r.runner.CacheKey; key != "" &&
		input.Settings.PromptCache.Key == "" {
		info, _ := r.runner.Client.ModelInfo(input.Model)
		if info.Capabilities.CacheKeyDefault {
			input.Settings.PromptCache.Key = key
		}
	}
	if finalReplyOnly {
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
	}
	// A request abandoned for an urgent steer before it is sent drops the
	// hook decisions of this pass; the next pass asks the hooks again.
	mark := r.hookMark()
	deliver, hidden, hookStop, ctxStop := r.beforeModel(s, &input, finalReplyOnly, purpose)
	// The model_call_started step records the tools offered, after any
	// hides, and the names hidden, so a call to a hidden tool is handled
	// from the record.
	defs = input.Tools
	if ctxStop != nil {
		return *ctxStop, false
	}
	if hookStop != nil {
		return r.stopWith(hookStop), false
	}
	if len(deliver) > 0 {
		if o := r.commit(Step{
			Kind:    StepMessagesAdded,
			Context: &ContextFact{Items: deliver},
		}); o != nil {
			return *o, false
		}
		mark = r.hookMark()
		if purpose == ModelPurposeCompaction {
			input.Messages, err = r.activeSummaryMessages(plan)
		} else {
			input.Messages, err = turnMessagesWithHistory(r.turn, r.history)
		}
		if err != nil {
			return r.stop(StopFailed, err.Error()), false
		}
	}
	if purpose == ModelPurposeCompaction {
		input.Settings.MaxTokens = r.summaryMaxTokens(input.Settings.MaxTokens)
		input.Settings.ToolChoice = nil
		input.Settings.ParallelTools = nil
		input.Settings.TaskBudget = nil
		input.Tools = nil
		defs = nil
	}
	if finalReplyOnly {
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
	if purpose == ModelPurposeCompaction {
		input.System += activeSummaryInstructions(s, input.Settings.MaxTokens)
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
		// The budget check before sending still uses the unchanged,
		// cautious worst-case estimate.
	} else if purpose == "" {
		if o, again, handled := r.guardActiveContext(s, input, req); handled {
			return o, again
		}
	} else if llm.InputTokenUpperBound(req) > max(0, s.ActiveCompactionWindow-req.Snapshot().Effective.MaxTokens) {
		return r.contextLimit(s, req, "summary evidence or hook context does not fit"), false
	}
	// The call runs under its own context, so an urgent steer can end it
	// without ending the run (U-6).
	callCtx, cancelCall := context.WithCancelCause(r.ctx)
	defer cancelCall(nil)
	never := ""
	switch {
	case tooldef.UnsafeProviderTool(defs):
		never = "it offers a provider tool that is not safe to repeat"
	case s.pausedProviderTool:
		// The steer could not follow the paused result anyway (FR-6).
		never = "it finishes a provider tool call the provider paused"
	}
	// An urgent steer never cuts a summary call. Cutting it would spend
	// the turn's one summary and leave the history too long to send; the
	// steer waits for the ordinary call after the summary instead.
	var call *steerableCall
	var back bool
	if purpose != ModelPurposeCompaction {
		call, back = r.registerCall(cancelCall, never)
	}
	defer r.dropCall(call)
	if back {
		r.dropHooks(mark)
		return RunResult{}, true
	}
	atRisk, remainingCost, costStop := checkSendBudget(s, req, r.agent.Model)
	if costStop != nil {
		return r.stopWith(costStop), false
	}
	terms, sendStop, ctxStop := r.beforeSend(s, req, atRisk, remainingCost, purpose)
	if ctxStop != nil {
		return *ctxStop, false
	}
	if r.asking != nil && sendStop == nil {
		// Asking a held turn: the hooks allow this call. Nothing is sent.
		r.asking.allowed = true
		return RunResult{}, false
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
		return RunResult{}, true
	}
	modelCall := fmt.Sprintf("%s:model:%d", r.turn.ID, s.modelCalls+1)
	messageID := modelCall + ":message"
	fact := &ModelFact{
		Purpose:              purpose,
		Compaction:           plan.clone(),
		ModelCallID:          modelCall,
		MessageID:            messageID,
		RequestHash:          canonjson.Hash(req.Body()),
		FinalReplyOnly:       finalReplyOnly,
		Reservation:          terms,
		WorstCaseCost:        atRisk,
		EstimatedInputTokens: llm.EstimatedInputTokens(req),
		Provider:             r.runner.Client.Provider(),
		API:                  r.runner.Client.API(),
		Model:                r.agent.Model,
		Effective:            snapshot.Effective,
		Endpoint:             snapshot.Origin,
		Adjustments:          snapshot.Adjustments,
		ContentChanges:       snapshot.ContentChanges,
		ModelInfo:            snapshot.ModelInfo,
		CapabilityPolicy:     snapshot.CapabilityPolicy,
		Unverified:           snapshot.Unverified,
		Tools:                defs,
		Hidden:               hidden,
	}
	if o := r.commit(Step{
		Kind:     StepModelCallStarted,
		Model:    fact,
		Warnings: budgetWarnings,
	}); o != nil {
		if stop := recorderLimit(r.err); stop != nil {
			// The recorder refused the start for a limit and wrote
			// nothing: record the stop as a BeforeSend limit would. The
			// start's budget clamp describes a call that never started
			// and is valid only on a start step, so it is not carried.
			r.err = nil
			return r.stopWith(stop), false
		}
		return *o, false
	}
	if err := r.ctx.Err(); err != nil || r.pauseRequested() {
		reason := "pause requested"
		if err != nil {
			reason = context.Cause(r.ctx).Error()
		}
		finished := &ModelFact{
			ModelCallID: modelCall,
			MessageID:   messageID,
			Error:       reason,
			Interrupted: InterruptRun,
			NotSent:     true,
		}
		if o := r.commit(Step{Kind: StepModelCallFinished, Model: finished}); o != nil {
			return *o, false
		}
		if err != nil {
			return r.contextStop(), false
		}
		return r.pauseStop(), false
	}
	if h := r.handOff(call, modelCall, messageID); h.done {
		return h.outcome, h.again
	}
	var result llm.Result
	var sendErr error
	var observedContent []llm.Content
	lastTextIndex := -1
	var observedUsage *llm.Usage
	for item, err := range llm.Stream(
		callCtx,
		r.runner.Client,
		llm.Call{
			Request: req,
			// The client may key durable work by this ID; see llm.CallIdentity.
			Identity: llm.CallIdentity{TurnID: r.turn.ID, ModelCallID: modelCall},
		},
	) {
		if item.Delta != nil {
			if item.Delta.Usage != nil {
				if observedUsage == nil {
					observedUsage = &llm.Usage{}
				}
				observedUsage.Absorb(item.Delta.Usage)
			}
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
					TurnID:      r.turn.ID,
					ModelCallID: modelCall,
					MessageID:   messageID,
					Delta:       item.Delta,
				})
			}
		}
		if item.Result != nil {
			result = *item.Result
			sendErr = err
		}
	}
	r.unregisterCall(call)
	// An interrupted call keeps what it produced and only the usage the
	// provider reported (U-9).
	partial := func(cause InterruptCause) *llm.Result {
		return interruptedResult(cause, result, observedContent, observedUsage)
	}
	var sendReason llm.ErrorReason
	if failure, ok := errors.AsType[*llm.Error](sendErr); ok {
		sendReason = failure.Reason
	}
	timeoutWon := sendReason == llm.ReasonResponseStartTimeout ||
		sendReason == llm.ReasonStreamIdleTimeout ||
		sendReason == llm.ReasonCallTimeout
	finished := &ModelFact{ModelCallID: modelCall, MessageID: messageID}
	// A Dashi timeout reason proves its own timer fired before the cut, so
	// the timeout wins and the steer is not added yet. A result that
	// completed as the cancel arrived also stands, and the steer waits for
	// the next call (U-7).
	if !timeoutWon && r.ctx.Err() == nil &&
		errors.Is(context.Cause(callCtx), errSteered) &&
		!completeResult(result, sendErr, r.agent) {
		return r.finishSteered(
			finished,
			partial(InterruptSteering),
			sendErr,
			result,
		)
	}
	if r.ctx.Err() != nil && !timeoutWon {
		finished.Interrupted = InterruptRun
		finished.Error = context.Cause(r.ctx).Error()
		finished.ResultUnknown = tooldef.UnsafeProviderTool(defs)
		finished.Result = partial(InterruptRun)
		step := Step{Kind: StepModelCallFinished, Model: finished}
		if o := r.commit(step); o != nil {
			return *o, false
		}
		if finished.ResultUnknown && r.runner.OnContextDone != ContextCancel &&
			!errors.Is(context.Cause(r.ctx), errTurnDeadline) {
			return r.stop(
				StopResultUnknown,
				"provider tool may have run",
			), false
		}
		return r.contextStop(), false
	}
	var validationErr error
	if sendErr != nil {
		// A failed call keeps what it streamed, with its reported usage.
		result = mergeObserved(result, observedContent, observedUsage)
		markIncomplete(&result)
		finished.Error = sendErr.Error()
		finished.ErrorReason = sendReason
		finished.SendStatus = sendStatusOf(sendErr, result)
		finished.ResultUnknown = finished.SendStatus == llm.SendStatusUnknown
		if failure, ok := errors.AsType[*llm.Error](sendErr); ok {
			finished.ErrorCategory = failure.Category
			finished.HTTPStatus = failure.Status
			finished.Retryable = failure.Retryable
			finished.RetryAfter = max(failure.RetryAfter, 0)
			if !finished.ResultUnknown {
				finished.Evidence = fmt.Sprintf(
					"client send status %s; HTTP %d",
					failure.SendStatus,
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
			finished.Error = validationErr.Error()
		}
	}
	if sendErr == nil ||
		result.Stop.Kind != "" ||
		result.ID != "" ||
		len(result.Message.Content) > 0 ||
		result.Usage != nil {
		finished.Result = &result
	}
	if o := r.commit(Step{Kind: StepModelCallFinished, Model: finished}); o != nil {
		return *o, false
	}
	if r.ctx.Err() != nil && !timeoutWon {
		return r.contextStop(), false
	}
	if sendErr != nil {
		if finished.ResultUnknown {
			return r.stop(StopResultUnknown, sendErr.Error()), false
		}
		if purpose == ModelPurposeCompaction {
			// The run loop applies a summary fallback.
			return RunResult{}, true
		}
		return r.stop(StopFailed, sendErr.Error()), false
	}
	if validationErr != nil {
		if purpose == ModelPurposeCompaction {
			// The run loop asks once for a shorter summary or applies a
			// summary fallback.
			return RunResult{}, true
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
		return RunResult{}, true
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
	outcome RunResult
	again   bool
}

// handOff marks call handed to the client, or, when an urgent steer cut it
// short after its request was recorded but before the hand-off, finishes it
// as not sent, so the run goes back for the steer.
func (r *runtime) handOff(
	call *steerableCall,
	modelCall string,
	messageID string,
) handOffResult {
	if call == nil {
		return handOffResult{}
	}
	// The listener gets its own view of the turn, built here with the
	// model_call_started step recorded, because it runs on another
	// goroutine: sharing the run's state would race with the run's commits.
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
	finished := &ModelFact{
		ModelCallID: modelCall,
		MessageID:   messageID,
		Error:       errSteered.Error(),
		Interrupted: InterruptSteering,
		NotSent:     true,
	}
	if o := r.commit(Step{Kind: StepModelCallFinished, Model: finished}); o != nil {
		return handOffResult{done: true, outcome: *o}
	}
	return handOffResult{done: true, again: true}
}

// finishSteered records a call an urgent steer cut short after the hand-off
// and goes back for the steer. The call counts as sent unless the client
// reports it was not sent: a missing response proves nothing. It
// does not count toward ModelCalls (U-8). It keeps its partial result and
// is charged only the usage the provider reported, like any other
// interrupted call (U-9, decision 47).
func (r *runtime) finishSteered(
	finished *ModelFact,
	partial *llm.Result,
	sendErr error,
	result llm.Result,
) (RunResult, bool) {
	finished.Interrupted = InterruptSteering
	finished.Error = errSteered.Error()
	// A SendStatusProviderRejected error that coincides with the cut
	// finishes as sent and is charged any usage it reported, which keeps the
	// rule simple.
	if sendStatusOf(sendErr, result) == llm.SendStatusNotSent {
		finished.NotSent = true
	} else {
		finished.Result = partial
	}
	if o := r.commit(Step{Kind: StepModelCallFinished, Model: finished}); o != nil {
		return *o, false
	}
	return RunResult{}, true
}

// recorderLimit returns the limited stop for a recorder that refused a
// model call start with a LimitError and wrote nothing, or nil.
func recorderLimit(err error) *Stop {
	ce, ok := errors.AsType[*CommitError](err)
	if !ok || !ce.NothingWritten {
		return nil
	}
	le, ok := errors.AsType[*LimitError](err)
	if !ok || !validHookLimit(le.Limit) {
		return nil
	}
	limit := le.Limit
	reason := le.Reason
	if reason == "" {
		reason = string(limit.Kind)
	}
	return &Stop{Kind: StopLimited, Reason: reason, Limit: &limit}
}
