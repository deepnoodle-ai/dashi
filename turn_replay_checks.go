package dashi

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

func (s *replayState) pending() []string {
	var out []string
	if s.modelCallStart != "" {
		out = append(out, s.modelCallStart)
	}
	if s.unresolvedModel != "" {
		out = append(out, s.unresolvedModel)
	}
	for _, id := range s.callOrder {
		if s.Tools[id].Status != ToolFinished {
			out = append(out, id)
		}
	}
	for _, id := range s.providerToolOrder {
		if _, ok := s.providerTool[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// pendingItems describes the work a host must act on. Calls that are only
// requested, allowed, or denied are left out: Continue alone runs them.
func (s *replayState) pendingItems() []Pending {
	var out []Pending
	for _, id := range []string{s.modelCallStart, s.unresolvedModel} {
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
			ModelCallID: s.modelCallOfTool[id],
			Name:        v.Call.Name,
			Args:        v.Call.Args,
			Tool:        &v,
		})
	}
	for _, id := range s.providerToolOrder {
		a, ok := s.providerTool[id]
		if !ok {
			continue
		}
		out = append(out, Pending{
			Kind:         PendingApproval,
			ID:           id,
			ModelCallID:  s.providerToolModelCalls[id],
			Name:         a.Name,
			Args:         a.Arguments,
			ProviderTool: &a,
		})
	}
	return out
}

// inferredStop is the stop a caller sees for a turn at rest. A record
// without a stop step ended mid-run, so the stop is inferred as Run would
// have recorded it and marked Inferred.
func (s *replayState) inferredStop() Stop {
	switch {
	case s.Stop.Kind != "":
		return s.Stop
	case s.unresolvedModel != "":
		return Stop{
			Kind:     StopResultUnknown,
			Reason:   "model call result unknown; the host must resolve it",
			Inferred: true,
		}
	case s.modelCallStart != "" || hasStartedTool(s):
		return Stop{
			Kind:     StopInterrupted,
			Reason:   "worker stopped before result",
			Inferred: true,
		}
	case len(s.Pending) > 0:
		return Stop{Kind: StopWaiting, Inferred: true}
	case len(s.pending()) > 0:
		// Requested or authorized calls wait only for a worker.
		return Stop{
			Kind:     StopInterrupted,
			Reason:   "worker stopped before result",
			Inferred: true,
		}
	}
	return Stop{Inferred: true}
}

func (s *replayState) recordResult(
	modelCall string,
	result *llm.Result,
	actionable bool,
) error {
	s.latestResult = result
	s.latestModelCall = modelCall
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
	s.pausedProviderTool = pausedMidProviderToolCall(*result)
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
			s.modelCallOfTool[c.ID] = modelCall
			s.callOrder = append(s.callOrder, c.ID)
		}
	}
	if result.Stop.Kind == llm.StopToolUse ||
		result.Stop.Kind == llm.StopFinished {
		for _, part := range result.Message.Content {
			if part.Kind == llm.ContentProviderToolApprovalRequest &&
				part.ProviderToolApprovalRequest != nil {
				a := part.ProviderToolApprovalRequest.Clone()
				if _, ok := s.providerTool[a.ID]; ok {
					return errors.New("repeated provider tool approval ID")
				}
				// A provider may reuse an ID once its earlier request was
				// resolved. The model call ID tells the two requests apart,
				// so each gets its own Resolve command.
				s.providerTool[a.ID] = a
				s.providerToolModelCalls[a.ID] = modelCall
				s.providerToolOrder = slices.DeleteFunc(
					s.providerToolOrder,
					func(id string) bool { return id == a.ID },
				)
				s.providerToolOrder = append(s.providerToolOrder, a.ID)
			}
		}
	}
	return nil
}

func (s *replayState) canContinue() bool {
	return s.canContinueWith(nil, 0)
}

// canContinueWith reports whether Continue can run, given the raised budget
// and model call limit it carries (nil and zero when it raises nothing).
func (s *replayState) canContinueWith(raise *Budget, raisedCap int) bool {
	if s.LastSeq == 0 || s.ended() || s.unresolvedModel != "" ||
		len(s.providerTool) > 0 {
		return false
	}
	budget := s.Budget
	if raise != nil {
		budget = raise
	}
	// Continue can resolve recorded work or record an ending stop after
	// a crash. The checks before each send prevent new work with
	// incomplete usage.
	if s.Resumes > max(s.Agent.Limits.Resumes, 0) {
		return false
	}
	if s.resumeLimitReached() {
		return false
	}
	if s.Stop.Kind == StopResultUnknown {
		// A recorded result_unknown stop means the host must report the
		// result. A missing stop means the worker may have crashed and
		// Continue may first establish which work can safely be repeated.
		if s.modelCallStart != "" {
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
		spent := s.ModelCalls >= max(s.modelCallCap, raisedCap)
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
			if s.ModelCalls >= max(s.modelCallCap, raisedCap) {
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

// terminal reports whether the turn has ended. It keeps an accepted ended
// stop even when a late result corrects its accounting. New stops check
// their Ended flag with stopEnds.
func (s *replayState) ended() bool {
	return s.Stop.Ended || s.stopEnds(s.Stop)
}

// stopEnds works out whether stop ends the turn, from the steps up to the
// proposed stop. It deliberately ignores the supplied Ended flag.
func (s *replayState) stopEnds(stop Stop) bool {
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
	if !s.Stop.Ended {
		return nil
	}
	if c, ok := command.(Resolve); ok &&
		c.Approval == nil && c.ProviderToolApproval == nil {
		if c.ModelResult != nil || c.ModelFailure != nil {
			if c.CallID != "" &&
				(c.CallID == s.modelCallStart || c.CallID == s.unresolvedModel) {
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
// that View would reject because it makes an open limited stop end the
// turn (checkStillOpen). A host recorder need not rebuild the view of what
// it commits, so without this the step would be stored and the turn would
// fail View from then on. It rebuilds the view only when the turn rests at
// an open limited stop.
func checkLateResultStep(turn Turn, s *replayState, step Step) error {
	if s.Stop.Kind != StopLimited || s.Stop.Ended {
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

// checkStillOpen enforces that a turn ends only through a recorded stop: a
// late result after a limited stop that did not end the turn must not end
// it, or the recorded Ended flag would be stale. The runner records a
// result_unknown stop before any budget stop while a model call has no
// result, so no record it writes fails this.
func (s *replayState) checkStillOpen() error {
	if s.Stop.Kind != StopLimited || s.Stop.Ended {
		return nil
	}
	if err := s.calculateSpend(); err != nil {
		return err
	}
	if s.ended() || incompleteUsageStop(s) != nil {
		return errors.New("late result ended the turn without a stop")
	}
	return nil
}

func (s *replayState) resumeLimitReached() bool {
	if s.LastSeq == 0 ||
		s.Resumes < max(s.Agent.Limits.Resumes, 0) {
		return false
	}
	return s.resumeCounts()
}

// resumeCounts reports whether continuing from the current stop counts
// toward Limits.Resumes. A crash (no stop) always counts. An
// interrupted stop counts once the turn has made a model call; one
// recorded before any, such as a Start recorded with a closed pause,
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

// trackHooks notes the hooks that made a decision in step; see HookActed.
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
// that was sent follows them, as TurnMessagesFull does.
// trackInput adds the step's input to Input: a start's input, and the
// user items of an AddMessages command or a steer.
func (s *replayState) trackInput(step Step) {
	if step.Kind == StepTurnStarted {
		s.Input = llmclone.Messages(step.Started.Input)
	} else if step.Context == nil ||
		!step.Context.Steering &&
			(step.Kind != StepMessagesAdded || step.CommandID == "") {
		return
	}
	for _, item := range step.Items() {
		if cmp.Or(item.Origin, ContextUser) != ContextUser {
			continue
		}
		message := item.Message.Clone()
		message.Context = nil
		if item.Name != "" {
			message.Context = &llm.MessageContext{
				Origin: string(ContextUser),
				Name:   item.Name,
			}
		}
		s.Input = append(s.Input, message)
	}
}

func (s *replayState) trackContext(step Step) {
	switch step.Kind {
	case StepTurnStarted, StepMessagesAdded, StepLoopContinued, StepToolFinished:
		for _, item := range step.Items() {
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
		if s.callStartSeq == nil {
			s.callStartSeq = map[string]int64{}
		}
		s.callStartSeq[step.Model.ModelCallID] = step.Seq
	case StepModelCallFinished, StepModelCallFinishedByHost:
		if callStart := s.startedModelCalls[step.Model.ModelCallID]; callStart != nil && callStart.Purpose == ModelPurposeCompaction {
			return
		}
		if !modelDispatched(step.Model) {
			return
		}
		callStart := s.callStartSeq[step.Model.ModelCallID]
		for key, item := range s.live {
			if item.NextCallOnly && s.liveSeq[key] < callStart {
				delete(s.live, key)
			}
		}
	}
}

// keptFinishCandidate reports a final text reply kept by a stop before it
// was accepted: a BeforeFinish failure, an interruption, or a worker that
// stopped with no stop recorded. Context added while it is kept is
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
// model call that made the call id, according to its model_call_started
// step.
func (s *replayState) hiddenAt(id, name string) bool {
	callStart := s.startedModelCalls[s.modelCallOfTool[id]]
	return callStart != nil && slices.Contains(callStart.Hidden, name)
}

// roomForAnyCall reports whether one more model call fits ModelCalls,
// the reserved final-reply call included. A call in flight counts as used.
func (s *replayState) roomForAnyCall() bool {
	used := s.ModelCalls
	if s.modelCallStart != "" {
		used++
	}
	return used < s.modelCallCap
}

// roomForNormalCall reports whether one more model call fits before the
// reserved final-reply call, or within ModelCalls when there is no
// reserve. A model call in flight, started but not finished, counts as
// used, since ModelCalls counts only finished calls.
func (s *replayState) roomForNormalCall() bool {
	used := s.ModelCalls
	if s.modelCallStart != "" {
		used++
	}
	limit := s.modelCallCap
	if s.Agent.Limits.NoReservedFinalReply {
		return used < limit
	}
	return used+1 < limit
}
