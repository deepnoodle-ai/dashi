package main

import (
	"cmp"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

type summaryPart struct{ index, content int }

func (r *Renderer) reasoningDelta(event dashi.Event) []Block {
	if event.Delta.Text == "" {
		return nil
	}
	id := "reasoning:" + messageKey(event.MessageID, event.ModelCallID)
	parts := r.reasoning[id]
	if parts == nil {
		parts = map[summaryPart]string{}
		r.reasoning[id] = parts
	}
	parts[summaryPart{event.Delta.Index, event.Delta.ContentIndex}] += event.Delta.Text
	keys := make([]summaryPart, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b summaryPart) int {
		if a.index != b.index {
			return cmp.Compare(a.index, b.index)
		}
		return cmp.Compare(a.content, b.content)
	})
	text := make([]string, 0, len(parts))
	for _, key := range keys {
		text = append(text, parts[key])
	}
	return []Block{{ID: id, Kind: KindReasoning, Text: strings.Join(text, "\n\n")}}
}
func (r *Renderer) reasoningSettled(step dashi.Step) []Block {
	if !r.showReasoning || r.Origin == dashi.StartCompaction || step.Model == nil {
		return nil
	}
	model := step.Model
	id := "reasoning:" + messageKey(model.MessageID, model.ModelCallID)
	var summaries []string
	if model.Result != nil {
		for _, part := range model.Result.Message.Content {
			if part.Kind == llm.ContentReasoning && part.Reasoning != nil && part.Reasoning.Text != "" {
				summaries = append(summaries, part.Reasoning.Text)
			}
		}
	}
	if len(summaries) == 0 && len(r.reasoning[id]) == 0 {
		return nil
	}
	delete(r.reasoning, id)
	return []Block{{ID: id, Kind: KindReasoning, Text: strings.Join(summaries, "\n\n"), Done: true}}
}
