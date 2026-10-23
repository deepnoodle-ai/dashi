package dashi

import (
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi/llm"
)

// callTaskBudget returns the task budget for an ordinary model call of t, as
// turnTaskBudget does. A budget that cannot be derived does not end the
// Turn: the call sends the agent's budget as it is, with a warning.
func callTaskBudget(t Turn, budget *llm.TaskBudget, memo map[int64]int) (*llm.TaskBudget, []Warning) {
	out, err := turnTaskBudget(t, budget, memo)
	if err == nil {
		return out, nil
	}
	out = cloneTaskBudget(budget)
	return out, []Warning{{
		Code:    WarningTaskBudgetAdjusted,
		Message: fmt.Sprintf("task budget remaining could not be derived after compaction (%v); sent the agent's budget unchanged", err),
	}}
}

// turnTaskBudget returns the task budget for an ordinary model call of t:
// a copy of the agent's budget, with Remaining set once active compaction has
// replaced messages of t.
//
// While every call sends the Turn's full history, the provider derives the
// Turn's spend from that history, so the budget goes as the agent set it.
// A compaction takes the replaced messages out of the request, and their
// spend with them, so Remaining carries it: the agent's Remaining, or Total
// when that is unset, less the tokens of the Turn's messages that are no
// longer sent, floored at 0.
//
// A Total the provider raised to its minimum on an earlier call of t counts
// from the raised Total, as the provider's raise does: the result has the
// raised Total and a Remaining raised by the same amount before the
// replaced tokens are taken off, so a budget below the minimum sends what
// the minimum would.
//
// It reads only the recorded Turn, so a recovered or replayed Turn computes
// the same value. memo, when not nil, keeps each compaction's count by the
// seq of its context_compacted step, so a run counts each compaction once.
func turnTaskBudget(t Turn, budget *llm.TaskBudget, memo map[int64]int) (*llm.TaskBudget, error) {
	if budget == nil {
		return nil, nil
	}
	out := cloneTaskBudget(budget)
	replaced, err := replacedTurnTokens(t, memo)
	if err != nil || replaced == 0 {
		return out, err
	}
	remaining := out.Total
	if out.Remaining != nil {
		remaining = *out.Remaining
	}
	if total := raisedTaskBudgetTotal(t, out.Total); total > out.Total {
		remaining += total - out.Total
		out.Total = total
	}
	out.Remaining = new(max(remaining-replaced, 0))
	return out, nil
}

// raisedTaskBudgetTotal returns the Total the provider sent in place of
// total on the last ordinary model call of t that raised it, as recorded in
// the call's task budget clamp, or 0 when no call raised it. A raise of a
// different Total, such as one a BeforeModel hook set, is not counted.
func raisedTaskBudgetTotal(t Turn, total int) int {
	raised := 0
	for _, step := range t.Steps {
		if step.Kind != StepModelCallStarted || step.Model == nil ||
			step.Model.Purpose != "" {
			continue
		}
		for _, a := range step.Model.Adjustments {
			if a.Field != llm.SettingTaskBudget || a.Action != llm.AdjustmentClamp {
				continue
			}
			var requested, effective llm.TaskBudget
			if json.Unmarshal(a.Requested, &requested) != nil ||
				json.Unmarshal(a.Effective, &effective) != nil {
				continue
			}
			if requested.Total == total && effective.Total > total {
				raised = effective.Total
			}
		}
	}
	return raised
}

// replacedTurnTokens counts the tokens of t's messages that active compaction
// has taken out of the request: the messages each context_compacted step's
// plan covered. A model reply counts the output tokens its call recorded,
// which include reasoning the message may not carry. Other messages, such as
// tool results and added context, are estimated with
// llm.EstimatedMessageTokens. A summary that a later compaction covers
// counts nothing: the messages it stood for were counted already.
func replacedTurnTokens(t Turn, memo map[int64]int) (int, error) {
	output := map[int64]int{}
	summaries := map[int64]bool{}
	plans := map[string]*ContextCompactionPlan{}
	var cache *contextValidationCache
	tokens := 0
	for _, step := range t.Steps {
		switch step.Kind {
		case StepModelCallStarted:
			if step.Model.Purpose == ModelPurposeCompaction {
				plans[step.Model.ModelCallID] = step.Model.Compaction
			}
		case StepModelCallFinished, StepModelCallFinishedByHost:
			if r := step.Model.Result; r != nil && r.Usage != nil {
				output[step.Seq] = r.Usage.OutputTokens
			}
		case StepContextCompacted:
			summaries[step.Seq] = true
			if n, ok := memo[step.Seq]; ok {
				tokens += n
				continue
			}
			if step.Compacted == nil {
				return 0, fmt.Errorf("context_compacted step %d has no fact", step.Seq)
			}
			plan := plans[step.Compacted.ModelCallID]
			if cache == nil {
				cache = &contextValidationCache{prefixes: map[int64][]contextMessage{}}
			}
			messages, err := coveredMessagesCached(t, plan, cache)
			if err != nil {
				return 0, err
			}
			n := 0
			for i, ref := range plan.Covered {
				m := messages[i]
				switch out, ok := output[ref.Seq]; {
				case summaries[ref.Seq]:
				case ok && m.Role == llm.RoleAssistant:
					n += out
				default:
					n += llm.EstimatedMessageTokens([]llm.Message{m})
				}
			}
			if memo != nil {
				memo[step.Seq] = n
			}
			tokens += n
		}
	}
	return tokens, nil
}

func cloneTaskBudget(b *llm.TaskBudget) *llm.TaskBudget {
	if b == nil {
		return nil
	}
	out := *b
	if b.Remaining != nil {
		out.Remaining = new(*b.Remaining)
	}
	return &out
}

// keepTaskBudgetRemaining lowers the Remaining a BeforeModel hook was given
// when the hook lowered Total below it and left Remaining as it was, so the
// hook's smaller budget is sent instead of failing validation. A Remaining
// the hook set itself is the hook's own and is not changed.
func keepTaskBudgetRemaining(given *llm.TaskBudget, s *llm.Settings, hook string) []Warning {
	b := s.TaskBudget
	if b == nil || b.Remaining == nil || *b.Remaining <= b.Total || b.Total <= 0 ||
		given == nil || given.Remaining == nil || *given.Remaining != *b.Remaining {
		return nil
	}
	fitted := cloneTaskBudget(b)
	fitted.Remaining = new(b.Total)
	s.TaskBudget = fitted
	return []Warning{{
		Code: WarningTaskBudgetAdjusted,
		Message: fmt.Sprintf(
			"BeforeModel hook %q lowered the task budget total to %d, below the remaining %d; sent remaining %d",
			hook, b.Total, *b.Remaining, b.Total,
		),
	}}
}
