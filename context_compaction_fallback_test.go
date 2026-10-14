package dashi

import (
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Replay accepts a summary fallback only for a failed or unapplied summary
// call, and only with the fallback note and no summary.
func TestActiveCompactionFallbackReplayChecks(t *testing.T) {
	c, a := activeFixture()
	c.results[2] = llm.Result{}
	c.errors = []error{nil, nil, &llm.Error{Provider: "test", Status: 529, SendStatus: llm.SendStatusProviderRejected}}
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "fallback-replay"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	ordinary := ""
	for i, step := range out.Turn.Steps {
		if step.Kind == StepModelCallStarted && step.Model.Purpose == "" && ordinary == "" {
			ordinary = step.Model.ModelCallID
		}
		if step.Kind == StepContextCompacted && step.Compacted.SummaryFallback != "" {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatal("no summary fallback recorded")
	}
	if _, err := replay(out.Turn); err != nil {
		t.Fatalf("recorded fallback does not replay: %v", err)
	}
	for name, change := range map[string]func(*ContextCompactionFact){
		"with summary":   func(f *ContextCompactionFact) { f.Summary = "both" },
		"without note":   func(f *ContextCompactionFact) { f.SummaryFallback = "earlier work" },
		"ordinary call":  func(f *ContextCompactionFact) { f.ModelCallID = ordinary },
		"negative after": func(f *ContextCompactionFact) { f.EstimatedAfter = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := out.Turn
			bad.Steps = slices.Clone(out.Turn.Steps[:at+1])
			fact := *bad.Steps[at].Compacted
			change(&fact)
			bad.Steps[at].Compacted = &fact
			if _, err := replay(bad); err == nil {
				t.Fatal("replay accepted an invalid summary fallback")
			}
		})
	}
}

// An in-turn summary fallback is sized like a compaction turn's: half the
// summary's output allowance, converted from tokens to bytes. Sized in
// tokens, it had room for little but the note.
func TestActiveCompactionFallbackSize(t *testing.T) {
	c, a := activeFixture()
	reply := "Plan: " + strings.Repeat("read the evidence, ", 200)
	c.results[1].Message.Content = append([]llm.Content{llm.Text(reply)}, c.results[1].Message.Content...)
	c.results[2] = llm.Result{}
	c.errors = []error{nil, nil, &llm.Error{Provider: "test", Status: 529, SendStatus: llm.SendStatusProviderRejected}}
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "fallback-size"}, activeStart(a))
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, out)
	got := out.ContextCompactions[0].SummaryFallback
	budget := compactiontext.FallbackBudget(a.ActiveCompaction.MaxTokens)
	// The reply is cut to fill the budget: more than half of it, and no
	// more than the budget plus a cut marker.
	if len(got) < budget/2 || len(got) > budget+64 {
		t.Fatalf("fallback is %d bytes, want about %d:\n%s", len(got), budget, got)
	}
	if !strings.Contains(got, "[your last reply]\nPlan: read the evidence") {
		t.Fatalf("fallback lacks the start of the last reply:\n%s", got)
	}
}
