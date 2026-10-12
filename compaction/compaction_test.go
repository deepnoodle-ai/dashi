package compaction

import (
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestIsSummary(t *testing.T) {
	history, _ := conversation(6, 400)
	s, err := (&Compactor{Policy: policyWith(20_000)}).Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	view := dashi.TurnView{
		Compaction: s.Compaction,
		Stop:       dashi.Stop{Kind: dashi.StopCompleted, Ended: true},
		Reply:      "<summary>Heron, port 8443.</summary>",
	}
	replaced, ok := view.ReplacementHistory()
	if !ok {
		t.Fatal("no replacement history")
	}
	summaries := 0
	for i, m := range replaced {
		if IsSummary(m) {
			summaries++
			if i != s.Compaction.SummaryAt {
				t.Fatalf("message %d reads as the summary; want %d", i, s.Compaction.SummaryAt)
			}
		}
	}
	if summaries != 1 {
		t.Fatalf("%d summaries in the replacement history", summaries)
	}
	failed := view
	failed.Stop, failed.Reply = dashi.Stop{Kind: dashi.StopFailed, Ended: true}, ""
	fallback, ok := failed.ReplacementHistory()
	if !ok || !IsSummary(fallback[s.Compaction.SummaryAt]) {
		t.Fatal("the summary fallback does not read as a summary")
	}
	empty := llm.UserMessage(llm.Text(compactiontext.EmptyFallback))
	if !IsSummary(empty) {
		t.Fatal("the fixed note does not read as a summary")
	}
	active := llm.UserMessage(llm.Text(compactiontext.ActiveFrame + "\n\nsettled work"))
	if !IsSummary(active) {
		t.Fatal("an active compaction summary does not read as one")
	}
	for name, m := range map[string]llm.Message{
		"plain user text": llm.UserMessage(llm.Text("Summarize this, please.")),
		"frame alone":     llm.UserMessage(llm.Text(summaryFrame)),
		"assistant":       {Role: llm.RoleAssistant, Content: active.Content},
		"other frame":     llm.UserMessage(llm.Text("Summary:\n\nnotes")),
	} {
		if IsSummary(m) {
			t.Errorf("%s reads as a summary", name)
		}
	}
}
