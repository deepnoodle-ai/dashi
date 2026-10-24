package dashi

import (
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// nextCallPossible is nextCallFits with a pause also refusing: steering
// uses it, since a steer not added to the turn stays in the inbox.
func (r *runtime) nextCallPossible(s *replayState, extraInputTokens int) bool {
	return !r.pauseRequested() && r.nextCallFits(s, extraInputTokens)
}

// nextCallFits reports whether a further normal model call can happen: one
// that still leaves the reserved final-reply call available (or fits in
// ModelCalls when there is no reserve), before the deadline, with the
// context live, and within every Budget cap for its worst case.
// extraInputTokens is the input the caller would add, such as added
// messages, estimated as llm.EstimatedMessageTokens does. A pause is
// not a reason to refuse: the run pauses before the next model call anyway,
// so a pause must never let a rejected call through.
//
// A model call in flight already counts as used, so the check holds while a
// call runs. The cost term estimates the next call from the latest call's
// reported or estimated input, grown by what the model and tools returned
// since and by extraInputTokens, priced like BeforeSend's check; a cost cap
// that cannot be priced fails it, since that call would stop anyway.
func (r *runtime) nextCallFits(s *replayState, extraInputTokens int) bool {
	return r.callFits(s, extraInputTokens, false)
}

// callFits is nextCallFits, with the reserved final-reply call counted as
// available when reserved is true: any call that fits ModelCalls.
func (r *runtime) callFits(
	s *replayState,
	extraInputTokens int,
	reserved bool,
) bool {
	if reserved {
		if !s.roomForAnyCall() {
			return false
		}
	} else if !s.roomForNormalCall() {
		return false
	}
	return r.callBlocked(s, extraInputTokens) == ""
}

// callBlocked is callFits without the ModelCalls term: it reports why no
// further model call fits the deadline, the run's context, and every Budget
// cap, or "" when one does. A model call in flight counts at its worst
// case against a cost cap, and a cap it cannot price blocks the call. It
// reads only r's immutable fields and s, so the steering listener can call
// it on a state captured for it.
func (r *runtime) callBlocked(s *replayState, extraInputTokens int) string {
	if d := r.agent.Limits.Deadline; !d.IsZero() && !time.Now().Before(d) {
		return "the turn deadline passed"
	}
	if r.ctx.Err() != nil {
		return "the run is ending"
	}
	if budgetSpentStop(s) != nil {
		return "the turn's budget is spent"
	}
	if s.Budget == nil || s.Budget.MaxCost == nil {
		return ""
	}
	req, ok := estimatedNextRequest(s, extraInputTokens)
	if !ok {
		return "the next call cannot be priced under the cost cap"
	}
	spent, ok := spendWithCallInFlight(s)
	if !ok {
		return "the call's worst-case cost is unknown under the cost cap"
	}
	if _, _, stop := checkSendBudget(spent, req, r.agent.Model); stop != nil {
		return "a replacement call would not fit the cost cap"
	}
	return ""
}

// spendWithCallInFlight returns s with a model call in flight counted at its
// worst case. Recorded spend counts an unfinished call as nothing, and the
// next call must fit beside everything the running one could still cost. It
// fails when that worst case is unknown.
func spendWithCallInFlight(s *replayState) (*replayState, bool) {
	if s.modelCallStart == "" {
		return s, true
	}
	callStart := s.startedModelCalls[s.modelCallStart]
	limit := s.Budget.MaxCost
	if callStart == nil || callStart.WorstCaseCost == nil ||
		callStart.WorstCaseCost.Currency != limit.Currency {
		return nil, false
	}
	used, known := s.Spend.costIn(limit.Currency)
	if !known {
		return nil, false
	}
	total, ok := used.Add(callStart.WorstCaseCost.Total)
	if !ok {
		return nil, false
	}
	adjusted := *s
	adjusted.Spend = s.Spend.Clone()
	if adjusted.Spend.Cost == nil {
		adjusted.Spend.Cost = map[string]money.Amount{}
	}
	adjusted.Spend.Cost[limit.Currency] = total
	return &adjusted, true
}

// estimatedNextRequest stands in for the next model request when pricing it
// before it exists. It carries no real body: its input estimate is set in
// RequestSnapshot.EstimatedInputTokens. The base is the latest real
// request: the input the provider reported for the latest call, or else the
// input estimated when that call, or the call in flight, started. What the
// next request adds is the in-flight call's max_tokens, or the latest
// result and its tool results, estimated by llm.EstimatedMessageTokens,
// plus extraInputTokens. Like every input estimate it can run low.
func estimatedNextRequest(s *replayState, extraInputTokens int) (llm.Request, bool) {
	var callStart *ModelFact
	tokens := 0
	if s.modelCallStart != "" {
		callStart = s.startedModelCalls[s.modelCallStart]
		if callStart != nil {
			tokens = cappedCount(
				max(callStart.EstimatedInputTokens, 0),
				max(callStart.Effective.MaxTokens, 0),
			)
		}
	} else {
		callStart = s.startedModelCalls[s.latestModelCall]
		if callStart != nil {
			tokens = max(callStart.EstimatedInputTokens, 0)
			latest, _ := modelCallCostOf(callStart, s.modelFinishes[s.latestModelCall])
			if latest.UsageSource == UsageSourceReported && latest.Usage != nil &&
				latest.Usage.TotalInputTokens() > 0 {
				tokens = latest.Usage.TotalInputTokens()
			}
		}
		var added []llm.Message
		if s.latestResult != nil {
			added = append(added, s.latestResult.Message)
		}
		for _, id := range s.callOrder {
			if s.modelCallOfTool[id] != s.latestModelCall {
				continue
			}
			if v := s.Tools[id]; v.Status == ToolFinished && v.Outcome != nil {
				result := v.Result()
				added = append(added, llm.Message{
					Role: llm.RoleUser,
					Content: []llm.Content{{
						Kind:       llm.ContentToolResult,
						ToolResult: &result,
					}},
				})
			}
		}
		tokens = cappedCount(tokens, llm.EstimatedMessageTokens(added))
	}
	if callStart == nil {
		return llm.Request{}, false
	}
	tokens = cappedCount(tokens, max(extraInputTokens, 0))
	// The next call's max_tokens is bounded by the output tokens left in the
	// budget, as the runner bounds it before sending.
	effective := callStart.Effective
	if s.Budget != nil && s.Budget.MaxOutputTokens > 0 {
		if remaining := s.Budget.MaxOutputTokens - s.Spend.Usage.OutputTokens; remaining > 0 &&
			(effective.MaxTokens == 0 || effective.MaxTokens > remaining) {
			effective.MaxTokens = remaining
		}
	}
	req, err := llm.NewRequest(llm.RequestSnapshot{
		Origin: callStart.Endpoint,
		Input: llm.Input{
			Model:    callStart.Model,
			Messages: []llm.Message{llm.UserMessage(llm.Text("estimate"))},
			Tools:    callStart.Tools,
			Settings: effective,
		},
		Effective:            effective,
		Body:                 []byte("{}"),
		ModelInfo:            callStart.ModelInfo,
		EstimatedInputTokens: max(tokens, 1),
	})
	return req, err == nil
}

// estimatedItemTokens estimates the input tokens context items add to a
// request, as llm.EstimatedMessageTokens does.
func estimatedItemTokens(items []ContextItem) int {
	messages := make([]llm.Message, 0, len(items))
	for _, item := range items {
		messages = append(messages, item.Message)
	}
	return llm.EstimatedMessageTokens(messages)
}
