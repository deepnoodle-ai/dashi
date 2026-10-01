package dashi

import (
	"encoding/json"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// nextCallPossible is nextCallFits with a pause also refusing: steering
// uses it, since a steer not added to the turn stays with its source.
func (r *runtime) nextCallPossible(s *replayState, extraInputTokens int) bool {
	return !r.pauseRequested() && r.nextCallFits(s, extraInputTokens)
}

// nextCallFits reports whether a further normal model call can happen: one
// that still leaves the reserved final-reply call available (or fits in
// ModelCalls when there is no reserve), before the deadline, with the
// context live, and within every Budget cap for its worst case.
// extraInputTokens is the size of input the caller would add, such as
// added messages, estimated as Dashi estimates request input. A pause is
// not a reason to refuse: the run pauses before the next model call anyway,
// so a pause must never let a rejected call through.
//
// A model call in flight already counts as used, so the check holds while a
// call runs. The cost term estimates the next call from the latest recorded
// request, grown by what the model and tools returned since and by
// extraInputTokens, priced like BeforeSend's check; a cost cap that cannot
// be priced fails it, since that call would stop anyway.
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
// worst case. Recorded spend counts an unfinished call at its estimated input
// only, and the next call must fit beside everything the running one could
// still cost. It fails when that worst case is unknown.
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
	counted := money.Amount(0)
	if _, cost, _, _, _ := modelCallCost(callStart, nil); cost != nil {
		if cost.Currency != limit.Currency {
			return nil, false
		}
		counted = cost.Total
	}
	used, known := s.Spend.costIn(limit.Currency)
	if !known {
		return nil, false
	}
	total, ok := used.Add(callStart.WorstCaseCost.Total - counted)
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
// before it exists. Its base is the latest real request: the call in flight,
// if any, or else the one that produced the latest result. What the next
// request adds is counted at twice its JSON size: the model's output since,
// bounded by the in-flight call's max_tokens or measured from the recorded
// result and tool outcomes, plus extraInputTokens. The doubling covers
// providers that encode such content once more, such as tool arguments sent
// as a JSON string, so the estimate stays an upper bound; the base is exact.
func estimatedNextRequest(s *replayState, extraInputTokens int) (llm.Request, bool) {
	grown := max(extraInputTokens, 0)
	var callStart *ModelFact
	if s.modelCallStart != "" {
		callStart = s.startedModelCalls[s.modelCallStart]
		if callStart != nil {
			grown += max(callStart.Effective.MaxTokens, 0)
		}
	} else {
		callStart = s.startedModelCalls[s.latestModelCall]
		if s.latestResult != nil {
			if b, err := json.Marshal(s.latestResult.Message); err == nil {
				grown += len(b)
			}
		}
		for _, id := range s.callOrder {
			if s.modelCallOfTool[id] != s.latestModelCall {
				continue
			}
			if o := s.Tools[id].Outcome; o != nil {
				if b, err := json.Marshal(o); err == nil {
					grown += len(b)
				}
			}
		}
	}
	if callStart == nil {
		return llm.Request{}, false
	}
	size := callStart.EstimatedInputTokens + 2*grown
	// EstimatedInputTokens is the body size plus 1024, so the body carries
	// the rest; a JSON string keeps it valid.
	bodySize := max(size-1024, 2)
	body := make([]byte, bodySize)
	body[0] = '"'
	for i := 1; i < bodySize-1; i++ {
		body[i] = 'x'
	}
	body[bodySize-1] = '"'
	req, err := llm.NewRequest(llm.RequestSnapshot{
		Origin: callStart.Endpoint,
		Input: llm.Input{
			Model:    callStart.Model,
			Messages: []llm.Message{llm.UserMessage(llm.Text("estimate"))},
			Tools:    callStart.Tools,
			Settings: callStart.Effective,
		},
		Effective: callStart.Effective,
		Body:      body,
		ModelInfo: callStart.ModelInfo,
	})
	return req, err == nil
}

// estimatedItemTokens sizes context items the way Dashi estimates request
// input: one token per byte of their encoding, an upper bound.
func estimatedItemTokens(items []ContextItem) int {
	n := 0
	for _, item := range items {
		if b, err := json.Marshal(item.Message); err == nil {
			n += len(b)
		}
	}
	return n
}
