package dashi

import "github.com/deepnoodle-ai/dashi/llm"

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
// It reads only the recorded Turn, so a recovered or replayed Turn computes
// the same value.
func turnTaskBudget(t Turn, budget *llm.TaskBudget) (*llm.TaskBudget, error) {
	if budget == nil {
		return nil, nil
	}
	out := *budget
	if budget.Remaining != nil {
		out.Remaining = new(*budget.Remaining)
	}
	replaced, err := replacedTurnTokens(t)
	if err != nil || replaced == 0 {
		return &out, err
	}
	remaining := out.Total
	if out.Remaining != nil {
		remaining = *out.Remaining
	}
	out.Remaining = new(max(remaining-replaced, 0))
	return &out, nil
}

// replacedTurnTokens counts the tokens of t's messages that active compaction
// has taken out of the request. A model reply counts the output tokens its
// call recorded, which include reasoning the message may not carry. Other
// messages, such as tool results and added context, are estimated with
// llm.EstimatedMessageTokens. A summary that a later compaction replaces
// counts nothing: the messages it stood for were counted already.
func replacedTurnTokens(t Turn) (int, error) {
	compacted := false
	output := map[int64]int{}
	for _, step := range t.Steps {
		switch step.Kind {
		case StepContextCompacted:
			compacted = true
		case StepModelCallFinished, StepModelCallFinishedByHost:
			if r := step.Model.Result; r != nil && r.Usage != nil {
				output[step.Seq] = r.Usage.OutputTokens
			}
		}
	}
	if !compacted {
		return 0, nil
	}
	cache := &contextValidationCache{prefixes: map[int64][]contextMessage{}}
	full, err := turnMessagesCached(t, nil, false, cache)
	if err != nil {
		return 0, err
	}
	sent, err := turnMessagesCached(t, nil, true, cache)
	if err != nil {
		return 0, err
	}
	kept := make(map[TurnMessageRef]bool, len(sent))
	for _, m := range sent {
		kept[m.Ref] = true
	}
	tokens := 0
	for _, m := range full {
		if kept[m.Ref] {
			continue
		}
		if n, ok := output[m.Ref.Seq]; ok && m.Message.Role == llm.RoleAssistant {
			tokens += n
			continue
		}
		tokens += llm.EstimatedMessageTokens([]llm.Message{m.Message})
	}
	return tokens, nil
}
