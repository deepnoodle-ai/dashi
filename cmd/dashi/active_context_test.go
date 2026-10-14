package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

func TestActiveContextRendererKeepsSummaryOutOfAnswers(t *testing.T) {
	r := NewRenderer(1, "fixture", "")
	r.showReasoning = true
	callStart := dashi.Step{Kind: dashi.StepModelCallStarted, TurnID: "turn", Model: &dashi.ModelFact{
		ModelCallID: "summary", Purpose: dashi.ModelPurposeCompaction,
		Compaction: &dashi.ContextCompactionPlan{EstimatedBefore: 12000},
	}}
	start := onlyBlock(t, r.Event(stepEvent(callStart)))
	if start.Kind != KindCompaction || start.Done {
		t.Fatalf("summary was not presented as running compaction: %+v", start)
	}
	for _, kind := range []llm.DeltaKind{llm.DeltaText, llm.DeltaReasoning} {
		if got := r.Event(dashi.Event{ModelCallID: "summary", Delta: &llm.Delta{Kind: kind, Text: "PRIVATE SUMMARY"}}); len(got) != 0 {
			t.Fatalf("summary preview leaked: %+v", got)
		}
	}
	finish := dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{
		ModelCallID: "summary", Result: &llm.Result{Message: llm.AssistantMessage(llm.Text("PRIVATE SUMMARY"))},
	}}
	// Purpose is inherited from the model_call_started step even if the
	// finish step omits it.
	for _, b := range r.Event(stepEvent(finish)) {
		if b.Kind != KindCompaction || strings.Contains(b.Text, "PRIVATE SUMMARY") {
			t.Fatalf("finished summary leaked: %+v", b)
		}
	}
	applied := onlyBlock(t, r.Event(stepEvent(dashi.Step{Kind: dashi.StepContextCompacted,
		Compacted: &dashi.ContextCompactionFact{ModelCallID: "summary", Summary: "PRIVATE SUMMARY", EstimatedAfter: 4000}})))
	compactionMustHave(t, applied.Text, "estimated input 12,000 → 4,000", "Full record retained")
	if !applied.Done || applied.State != StateOK || applied.ID != start.ID {
		t.Fatalf("applied marker lost identity: %+v", applied)
	}
	// Even a late preview remains an internal summary, never a new answer.
	if got := r.Event(dashi.Event{ModelCallID: "summary", Delta: &llm.Delta{Kind: llm.DeltaText, Text: "PRIVATE SUMMARY"}}); len(got) != 0 {
		t.Fatal("late summary preview leaked")
	}
	answer := onlyBlock(t, r.Event(dashi.Event{ModelCallID: "ordinary", Delta: &llm.Delta{Kind: llm.DeltaText, Text: "Actual answer"}}))
	if answer.Kind != KindAssistant || answer.Text != "Actual answer" {
		t.Fatalf("ordinary answer hidden: %+v", answer)
	}
	footer := r.Footer(dashi.TurnView{ModelCalls: 3, Spend: dashi.Spend{
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 50}, Cost: map[string]money.Amount{"USD": money.MustParseAmount("0.05")},
	}}, nil)
	compactionMustHave(t, footer.Text, "3 model calls", "150 tokens", "$0.050")
}

func TestActiveContextRendererFailureUnavailableAndStops(t *testing.T) {
	r := NewRenderer(1, "fixture", "")
	start := dashi.Step{Kind: dashi.StepTurnStarted, TurnID: "turn", Started: &dashi.StartFact{
		Agent: dashi.Agent{ActiveCompaction: &dashi.ActiveCompactionPolicy{}}, ActiveCompactionWindow: 16000,
	}}
	if blocks := r.Event(stepEvent(start)); len(blocks) != 1 || blocks[0].Kind != KindUser {
		t.Fatalf("start showed a per-Turn protection notice: %+v", blocks)
	}
	r.Event(stepEvent(dashi.Step{Kind: dashi.StepModelCallStarted, Model: &dashi.ModelFact{ModelCallID: "bad", Purpose: dashi.ModelPurposeCompaction}}))
	failure := onlyBlock(t, r.Event(stepEvent(dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{
		ModelCallID: "bad", Purpose: dashi.ModelPurposeCompaction, Error: "summary did not shrink",
	}})))
	compactionMustHave(t, failure.Text, "summary failed", "summary did not shrink")
	if failure.State != StateFailed || !failure.Done {
		t.Fatalf("invalid failed marker: %+v", failure)
	}
	fallback := onlyBlock(t, r.Event(stepEvent(dashi.Step{Kind: dashi.StepContextCompacted, Compacted: &dashi.ContextCompactionFact{
		ModelCallID: "bad", SummaryFallback: "Your conversation history was shortened without a summary.", EstimatedAfter: 900,
	}})))
	compactionMustHave(t, fallback.Text, "summary failed", "without a summary")
	if fallback.ID != failure.ID || !fallback.Done {
		t.Fatalf("fallback did not replace the failed marker: %+v", fallback)
	}
	unavailable := onlyBlock(t, r.Event(stepEvent(dashi.Step{TurnID: "unknown", Warnings: []dashi.Warning{{
		Code: dashi.WarningActiveCompactionUnavailable, Message: "Active-Turn protection unavailable; choose a model with a known window.",
	}}})))
	if unavailable.State != "unavailable" || strings.Contains(unavailable.Text, "200,000") {
		t.Fatalf("unavailable window became a safety claim: %+v", unavailable)
	}
	got := limitText(&dashi.Stop{Kind: dashi.StopLimited, Limit: &dashi.LimitDetail{Kind: dashi.LimitContextWindow}})
	compactionMustHave(t, got, "recorded context window", "new Turn", "Full work is retained")
	if strings.Contains(got, "Press Enter") {
		t.Fatal("context-window remedy claimed an implicit model change")
	}
}

func TestCLIActiveContextWindowPinnedPerRecordedStart(t *testing.T) {
	h := newHarness(t, say("First."), say("Second."))
	h.client.mu.Lock()
	h.client.window = 20000
	h.client.mu.Unlock()
	h.input("first request")
	h.client.mu.Lock()
	h.client.window = 50000
	h.client.mu.Unlock()
	h.input("second request")
	for i, want := range []int{20000, 50000} {
		start := h.turn(i + 1).Steps[0].Started
		if start.Agent.ActiveCompaction == nil || start.Agent.ActiveCompaction.Window != 0 || start.ActiveCompactionWindow != want {
			t.Fatalf("start %d lost policy/window: %+v", i+1, start)
		}
	}
	if strings.Contains(h.ui.text(), "recorded window") {
		t.Fatalf("per-Turn protection notice shown:\n%s", h.ui.text())
	}
	// Stored history retains the recorded window after catalog changes.
	if start := h.turn(1).Steps[0].Started; start.ActiveCompactionWindow != 20000 {
		t.Fatal("later catalog mutated first start")
	}
}

func TestActiveContextMarkerUsesAuthoritativeStop(t *testing.T) {
	for _, kind := range []dashi.StopKind{dashi.StopInterrupted, dashi.StopCanceled, dashi.StopLimited} {
		t.Run(string(kind), func(t *testing.T) {
			r := NewRenderer(1, "fixture", "")
			r.Event(stepEvent(dashi.Step{Kind: dashi.StepModelCallStarted, Model: &dashi.ModelFact{ModelCallID: "summary", Purpose: dashi.ModelPurposeCompaction}}))
			r.Event(stepEvent(dashi.Step{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{ModelCallID: "summary", Interrupted: "context ended"}}))
			blocks := r.activeContextStopped(dashi.Stop{Kind: kind})
			if len(blocks) != 1 || !blocks[0].Done || strings.Contains(blocks[0].Text, "Press Enter") {
				t.Fatalf("marker invented continuation: %+v", blocks)
			}
			if kind == dashi.StopCanceled && !strings.Contains(blocks[0].Text, "canceled") {
				t.Fatalf("cancellation masked: %+v", blocks)
			}
			if next := r.activeContextStopped(dashi.Stop{Kind: dashi.StopCompleted}); len(next) != 0 {
				t.Fatalf("later outcome rewrote historical marker: %+v", next)
			}
		})
	}
}
