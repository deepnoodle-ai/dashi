package dashi

import (
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/llm"
)

func (s *replayState) pending() []string {
	var out []string
	if s.modelIntent != "" {
		out = append(out, s.modelIntent)
	}
	if s.unresolvedModel != "" {
		out = append(out, s.unresolvedModel)
	}
	for _, id := range s.callOrder {
		if s.Tools[id].Status != ToolFinished {
			out = append(out, id)
		}
	}
	for _, id := range s.hostedOrder {
		if _, ok := s.hosted[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// pendingItems describes the work a host must act on. Calls that are only
// requested, allowed, or denied are left out: Continue alone runs them.
func (s *replayState) pendingItems() []Pending {
	var out []Pending
	for _, id := range []string{s.modelIntent, s.unresolvedModel} {
		if id != "" {
			out = append(out, Pending{Kind: PendingModel, ID: id, ModelCallID: id})
		}
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		kind := PendingTool
		switch v.Status {
		case ToolApprovalPending:
			kind = PendingApproval
		case ToolStarted, ToolWaiting, ToolResultUnknown:
		default:
			continue
		}
		out = append(out, Pending{
			Kind:        kind,
			ID:          id,
			ModelCallID: s.callAttempts[id],
			Name:        v.Call.Name,
			Args:        v.Call.Args,
			Tool:        &v,
		})
	}
	for _, id := range s.hostedOrder {
		a, ok := s.hosted[id]
		if !ok {
			continue
		}
		out = append(out, Pending{
			Kind:        PendingApproval,
			ID:          id,
			ModelCallID: s.hostedAttempts[id],
			Name:        a.Name,
			Args:        a.Arguments,
			Hosted:      &a,
		})
	}
	return out
}

// projectedStop is the stop a caller sees for a turn at rest. A record
// without a stop step ended mid-run, so the stop is inferred as Run would
// have recorded it and marked Projected.
func (s *replayState) projectedStop() Stop {
	switch {
	case s.Stop.Kind != "":
		return s.Stop
	case s.unresolvedModel != "":
		return Stop{
			Kind:      StopResultUnknown,
			Reason:    "model attempt needs reconciliation",
			Projected: true,
		}
	case s.modelIntent != "" || hasStartedTool(s):
		return Stop{
			Kind:      StopInterrupted,
			Reason:    "worker stopped before result",
			Projected: true,
		}
	case len(s.Pending) > 0:
		return Stop{Kind: StopWaiting, Projected: true}
	case len(s.pending()) > 0:
		// Requested or authorized calls wait only for a worker.
		return Stop{
			Kind:      StopInterrupted,
			Reason:    "worker stopped before result",
			Projected: true,
		}
	}
	return Stop{Projected: true}
}

func (s *replayState) recordResult(
	attempt string,
	result *llm.Result,
	actionable bool,
) error {
	s.latestResult = result
	s.latestAttempt = attempt
	if !actionable {
		return nil
	}
	s.answerVetoed = false
	if err := validateModelResult(*result, s.Agent); err != nil {
		return err
	}
	if result.Stop.Kind == llm.StopPause {
		s.ProviderPauses++
	}
	s.pausedHosted = pausedMidHostedCall(*result)
	if result.Stop.Kind == llm.StopToolUse ||
		result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind != llm.ContentToolCall || part.ToolCall == nil {
				continue
			}
			c := part.ToolCall.Clone()
			if _, ok := s.Tools[c.ID]; ok {
				return errors.New("repeated tool call ID")
			}
			s.Tools[c.ID] = ToolView{
				Call:       c,
				Definition: s.modelDefs[c.Name],
				Status:     ToolRequested,
			}
			s.callAttempts[c.ID] = attempt
			s.callOrder = append(s.callOrder, c.ID)
		}
	}
	if result.Stop.Kind == llm.StopToolUse ||
		result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind == llm.ContentHostedApprovalRequest &&
				part.HostedApprovalRequest != nil {
				a := part.HostedApprovalRequest.Clone()
				if _, ok := s.hosted[a.ID]; ok {
					return errors.New("repeated hosted approval ID")
				}
				// A provider may reuse an ID once its earlier request was
				// answered. The attempt tells the two requests apart, so
				// each gets its own answer command.
				s.hosted[a.ID] = a
				s.hostedAttempts[a.ID] = attempt
				s.hostedOrder = slices.DeleteFunc(
					s.hostedOrder,
					func(id string) bool { return id == a.ID },
				)
				s.hostedOrder = append(s.hostedOrder, a.ID)
			}
		}
	}
	return nil
}

func (s *replayState) canContinue() bool {
	return s.canContinueWith(nil, 0)
}

// canContinueWith reports whether Continue can run, given the raised budget
// and model-attempt cap it carries (nil and zero when it raises nothing).
func (s *replayState) canContinueWith(raise *Budget, attempts int) bool {
	if s.Revision == 0 || s.terminal() || s.unresolvedModel != "" ||
		len(s.hosted) > 0 {
		return false
	}
	budget := s.Budget
	if raise != nil {
		budget = raise
	}
	// Continue can reconcile recorded work or record a terminal stop after
	// a crash. Dispatch guards prevent fresh work with incomplete usage.
	if s.Resumes > max(s.Agent.Limits.Resumes, 0) {
		return false
	}
	if s.interruptionLimitReached() {
		return false
	}
	if s.Stop.Kind == StopResultUnknown {
		// An explicit uncertain stop is evidence that the host must report
		// the outcome. A missing stop means the worker may have crashed and
		// Continue may first establish which work can safely be repeated.
		if s.modelIntent != "" {
			return false
		}
		for _, id := range s.callOrder {
			if s.Tools[id].Status == ToolStarted {
				return false
			}
		}
	}
	if s.Stop.Kind == StopLimited && s.Stop.Limit != nil {
		limit := s.Stop.Limit
		spent := s.ModelCalls >= max(s.attemptCap, attempts)
		switch limit.Kind {
		case LimitCost, LimitOutputTokens, LimitTotalTokens:
			// A budget raise alone cannot run pending tools when no model
			// call is left to read their results.
			if spent && s.Output == nil {
				return false
			}
		}
		switch limit.Kind {
		case LimitModelCalls:
			if s.ModelCalls >= max(s.attemptCap, attempts) {
				return false
			}
		case LimitCost:
			if s.Spend.CostUnknown > 0 {
				return false
			}
			if limit.UnpricedModel != "" {
				break
			}
			if budget == nil || budget.MaxCost == nil {
				return false
			}
			used, known := s.Spend.costIn(budget.MaxCost.Currency)
			after, ok := used.Add(limit.WorstCaseCost)
			if !known || !ok || limit.WorstCaseCost > 0 &&
				after > budget.MaxCost.Amount ||
				limit.WorstCaseCost == 0 && used >= budget.MaxCost.Amount {
				return false
			}
		case LimitOutputTokens:
			if budget == nil || budget.MaxOutputTokens <=
				s.Spend.Usage.OutputTokens {
				return false
			}
		case LimitTotalTokens:
			total := s.Spend.Usage.TotalInputTokens() +
				s.Spend.Usage.OutputTokens
			if budget == nil || budget.MaxTotalTokens <= total {
				return false
			}
		}
	}
	for _, id := range s.callOrder {
		v := s.Tools[id]
		switch v.Status {
		case ToolFinished, ToolRequested, ToolAllowed, ToolDenied:
		case ToolStarted:
		default:
			return false
		}
	}
	return true
}

// terminal preserves an accepted ended stop even when late reconciliation
// corrects its accounting. New stops validate their flag with stopTerminal.
func (s *replayState) terminal() bool {
	return s.Stop.Terminal || s.stopTerminal(s.Stop)
}

// stopTerminal derives terminality from the facts at the proposed stop's
// prefix. It deliberately ignores the supplied Terminal flag.
func (s *replayState) stopTerminal(stop Stop) bool {
	switch stop.Kind {
	case StopCompleted, StopCanceled:
		return true
	case StopLimited:
	default:
		return false
	}
	if stop.Limit == nil {
		return false
	}
	switch stop.Limit.Kind {
	case LimitDeadline, LimitResumes:
		return true
	case LimitCost:
		return s.Spend.CostUnknown > 0
	case LimitOutputTokens:
		return s.Spend.UsageIncomplete && s.Budget != nil &&
			s.Budget.MaxOutputTokens > 0
	case LimitTotalTokens:
		return s.Spend.UsageIncomplete && s.Budget != nil &&
			s.Budget.MaxTotalTokens > 0
	}
	return false
}

// acceptsAfterEnd rejects a command on an ended turn unless it is a Resolve
// reporting the result of work that had already started.
func (s *replayState) acceptsAfterEnd(command Command) error {
	if !s.Stop.Terminal {
		return nil
	}
	if c, ok := command.(Resolve); ok &&
		c.Approval == nil && c.HostedApproval == nil {
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID != "" &&
				(c.CallID == s.modelIntent || c.CallID == s.unresolvedModel) {
				return nil
			}
		} else if v, ok := s.Tools[c.CallID]; ok &&
			(v.Status == ToolStarted || v.Status == ToolWaiting ||
				v.Status == ToolResultUnknown) {
			return nil
		}
	}
	if s.Stop.Reason != "" {
		return fmt.Errorf("%w: %s (%s)", ErrTurnEnded, s.Stop.Kind, s.Stop.Reason)
	}
	return fmt.Errorf("%w: %s", ErrTurnEnded, s.Stop.Kind)
}

// checkLateResultStep refuses, before anything is saved, a late result
// that replay would reject because it makes an open limited stop terminal
// (checkStillOpen). A host recorder need not replay what it commits, so
// without this the step would be stored and the turn would fail replay from
// then on. It replays only when the turn rests at an open limited stop.
func checkLateResultStep(turn Turn, s *replayState, step Step) error {
	if s.Stop.Kind != StopLimited || s.Stop.Terminal {
		return nil
	}
	step.Version = 1
	step.TurnID = turn.ID
	step.Seq = int64(len(turn.Steps) + 1)
	candidate := turn
	candidate.Steps = append(slices.Clone(turn.Steps), step)
	if _, err := replay(candidate); err != nil {
		return fmt.Errorf("dashi: late result refused: %w", err)
	}
	return nil
}

// checkStillOpen enforces that a turn becomes non-continuable only through
// a recorded stop: a late result after a limited stop that was not
// terminal must not make it terminal, or the recorded Terminal would be
// stale. The runner records uncertain before any budget stop while a model
// call is unresolved, so no record it writes fails this.
func (s *replayState) checkStillOpen() error {
	if s.Stop.Kind != StopLimited || s.Stop.Terminal {
		return nil
	}
	if err := s.calculateSpend(); err != nil {
		return err
	}
	if s.terminal() || incompleteUsageStop(s) != nil {
		return errors.New("late result ended the turn without a stop")
	}
	return nil
}

func (s *replayState) interruptionLimitReached() bool {
	if s.Revision == 0 ||
		s.Resumes < max(s.Agent.Limits.Resumes, 0) {
		return false
	}
	return s.resumeCounts()
}

// resumeCounts reports whether continuing from the current stop counts
// toward Limits.Interruptions. A crash (no stop) always counts. An
// interrupted stop counts once the turn has made a model attempt; one
// recorded before any, such as a Start admitted with a closed pause,
// interrupted no work.
func (s *replayState) resumeCounts() bool {
	switch s.Stop.Kind {
	case "":
		return true
	case StopInterrupted:
		return s.modelCalls > 0
	}
	return false
}

func validateModelAccounting(fact *ModelFact) error {
	if !fact.Interrupted.valid() {
		return errors.New("unknown model interrupt cause")
	}
	if fact.NotSent && (fact.Interrupted == InterruptNone ||
		fact.Result != nil || fact.Cost != nil || fact.ResultUnknown) {
		return errors.New("known nondispatch cannot have a model result or charge")
	}
	if fact.Result != nil {
		if fact.Result.Usage != nil {
			if err := fact.Result.Usage.Validate(); err != nil {
				return err
			}
		}
		if fact.Result.Cost != nil {
			if err := fact.Result.Cost.Validate(); err != nil {
				return err
			}
		}
	}
	if fact.Cost != nil {
		if err := fact.Cost.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// trackHooks notes the hooks that answered in step; see HookActed.
func (s *replayState) trackHooks(step Step) {
	for _, r := range step.HookResults {
		if r.Verdict == VerdictFailed {
			continue
		}
		if s.acted == nil {
			s.acted = map[hookKey]bool{}
		}
		s.acted[hookKey{r.Point, r.Name}] = true
	}
}

// trackContext keeps the named items the next model call would see: the
// latest item per origin and name, less NextCallOnly items once a model call
// that was dispatched follows them, as Project does.
func (s *replayState) trackContext(step Step) {
	switch step.Kind {
	case StepMessagesAdded, StepLoopContinued, StepToolFinished:
		if step.Context == nil {
			return
		}
		for _, item := range step.Context.Items {
			if item.Name == "" {
				continue
			}
			if s.live == nil {
				s.live = map[string]ContextItem{}
				s.liveSeq = map[string]int64{}
			}
			key := contextKey(item)
			s.live[key] = cloneContextItem(item)
			s.liveSeq[key] = step.Seq
		}
	case StepModelCallStarted:
		if s.intentSeq == nil {
			s.intentSeq = map[string]int64{}
		}
		s.intentSeq[step.Model.ModelCallID] = step.Seq
	case StepModelCallFinished, StepModelCallFinishedByHost:
		if intent := s.modelIntents[step.Model.ModelCallID]; intent != nil && intent.Purpose == ModelPurposeCompaction {
			return
		}
		if !modelDispatched(step.Model) {
			return
		}
		intent := s.intentSeq[step.Model.ModelCallID]
		for key, item := range s.live {
			if item.NextCallOnly && s.liveSeq[key] < intent {
				delete(s.live, key)
			}
		}
	}
}

// keptFinishCandidate reports a final text answer kept by a stop before it
// was accepted: a BeforeFinish failure, an interruption, or a worker that
// stopped with no stop recorded. Context delivered while it is kept is
// counted in deliveredOnKept, and Continue decides at run time whether the
// model can be asked again.
func (s *replayState) keptFinishCandidate() bool {
	if !s.awaitingClassification {
		return false
	}
	switch s.Stop.Kind {
	case StopInterrupted, "":
		return true
	case StopFailed:
		return s.Stop.Reason == StopReasonHookFailed
	}
	return false
}

// trackIDs records the step's context item IDs. An ID may appear once in a
// turn: the runner skips a repeat before recording it.
func (s *replayState) trackIDs(step Step) error {
	for _, item := range step.Items() {
		if item.ID == "" {
			continue
		}
		if _, ok := s.contextIDs[item.ID]; ok {
			return fmt.Errorf("repeated context item ID %q", item.ID)
		}
		if s.contextIDs == nil {
			s.contextIDs = map[string]ContextItem{}
		}
		s.contextIDs[item.ID] = item
		if s.usedContextIDs == nil {
			s.usedContextIDs = map[string]bool{}
		}
		s.usedContextIDs[item.ID] = true
	}
	return nil
}

// hiddenAt reports whether a BeforeModel hook hid the named tool from the
// model call that made the call id, according to its recorded intent.
func (s *replayState) hiddenAt(id, name string) bool {
	intent := s.modelIntents[s.callAttempts[id]]
	return intent != nil && slices.Contains(intent.Hidden, name)
}

// roomForAnyCall reports whether one more model call fits ModelAttempts,
// the reserved final-answer call included. A call in flight counts as used.
func (s *replayState) roomForAnyCall() bool {
	used := s.ModelCalls
	if s.modelIntent != "" {
		used++
	}
	return used < s.attemptCap
}

// roomForNormalCall reports whether one more model call fits before the
// reserved final-answer call, or within ModelAttempts when there is no
// reserve. A model call in flight, requested but not settled, counts as
// used, since ModelAttempts counts only settled calls.
func (s *replayState) roomForNormalCall() bool {
	used := s.ModelCalls
	if s.modelIntent != "" {
		used++
	}
	limit := s.attemptCap
	if s.Agent.Limits.NoReservedFinalReply {
		return used < limit
	}
	return used+1 < limit
}
