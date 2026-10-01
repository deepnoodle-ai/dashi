package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestReasoningPreviewReconcilesWithoutPrivateText(t *testing.T) {
	r := NewRenderer(1, "fixture", t.TempDir())
	r.Event(dashi.Event{Step: &dashi.Step{Kind: dashi.StepTurnStarted, Started: &dashi.StartFact{Agent: dashi.Agent{Settings: llm.Settings{ReasoningSummary: new(true)}}}}})
	var last Block
	for _, delta := range []llm.Delta{{Kind: llm.DeltaReasoning, Index: 2, ContentIndex: 1, Text: "second"}, {Kind: llm.DeltaReasoning, Index: 2, ContentIndex: 0, Text: "first"}} {
		blocks := r.Event(dashi.Event{AttemptID: "attempt", MessageID: "message", Delta: &delta})
		if len(blocks) != 1 || blocks[0].Kind != KindReasoning {
			t.Fatalf("preview = %+v", blocks)
		}
		last = blocks[0]
	}
	if last.Text != "first\n\nsecond" {
		t.Fatalf("part order=%q", last.Text)
	}
	result := &llm.Result{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{
		{Kind: llm.ContentOpaque, Opaque: &llm.Opaque{Raw: []byte(`{"private":"NEVER_DISPLAY"}`)}},
		{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: "settled summary"}, Replay: &llm.Opaque{Raw: []byte(`{"private":"NEVER_DISPLAY"}`)}},
		llm.Text("answer"),
	}}}
	blocks := r.Event(dashi.Event{Step: &dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{AttemptID: "attempt", MessageID: "message", Result: result}}})
	if len(blocks) != 2 || blocks[0].ID != last.ID || blocks[0].Text != "settled summary" || !blocks[0].Done || blocks[1].Kind != KindAssistant || blocks[1].Text != "answer" {
		t.Fatalf("reconciliation = %+v", blocks)
	}
	for _, b := range blocks {
		if strings.Contains(FormatBlock(b), "NEVER_DISPLAY") {
			t.Fatal("private replay exposed")
		}
	}
	// Reopening rebuilds the same settled blocks without provisional duplicates.
	reopened := NewRenderer(1, "fixture", t.TempDir())
	reopened.showReasoning = true
	settled := reopened.Event(dashi.Event{Step: &dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{AttemptID: "attempt", MessageID: "message", Result: result}}})
	if len(settled) != 2 || !reflect.DeepEqual(settled[0], blocks[0]) {
		t.Fatalf("reopen = %+v", settled)
	}
}

func TestThinkingOffAndAbsentSummaryClearDisplay(t *testing.T) {
	r := NewRenderer(1, "fixture", t.TempDir())
	if blocks := r.Event(dashi.Event{AttemptID: "a", Delta: &llm.Delta{Kind: llm.DeltaReasoning, Text: "unsolicited"}}); len(blocks) != 0 {
		t.Fatal("unset thinking displayed")
	}
	r.showReasoning = true
	preview := r.Event(dashi.Event{AttemptID: "a", Delta: &llm.Delta{Kind: llm.DeltaReasoning, Text: "preview"}})
	settled := r.Event(dashi.Event{Step: &dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{AttemptID: "a", Result: &llm.Result{Message: llm.Message{Content: []llm.Content{llm.Text("answer")}}}}}})
	if len(settled) != 2 || settled[0].ID != preview[0].ID || settled[0].Text != "" || !settled[0].Done {
		t.Fatalf("absent final = %+v", settled)
	}
}
