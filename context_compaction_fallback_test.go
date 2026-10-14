package dashi

import (
	"slices"
	"testing"

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
