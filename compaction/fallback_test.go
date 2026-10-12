package compaction

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/llm"
)

// The fallback holds the user's messages, the tools called (repeats
// counted), and the last reply, within its budget.
func TestSummaryFallback(t *testing.T) {
	history, _ := conversation(3, 50)
	got := summaryFallback(history, len(history), 10_000)
	for _, want := range []string{
		compactiontext.FallbackNote,
		"12 earlier messages were removed",
		"[user]\nprompt 0",
		"[user]\nprompt 2",
		"[tools called] read (2 times)",
		"[your last reply]\nanswer 2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "answer 0") {
		t.Fatal("the fallback holds a reply other than the last")
	}

	big, _ := conversation(40, 5_000)
	got = summaryFallback(big, len(big), 4_000)
	if len(got) > 4_000+500 || !strings.Contains(got, "prompt 0") ||
		!strings.Contains(got, "answer 39") {
		t.Fatalf("fallback of %d bytes:\n%s", len(got), got)
	}
}

// Start's fallback is cut to half the summary's output allowance.
func TestStartFallbackWithinHalfTheAllowance(t *testing.T) {
	history, _ := conversation(20, 4_000)
	s, err := (&Compactor{Policy: dueAtWindow(40_000), TailFraction: -1}).
		Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	limit := s.Agent.Settings.MaxTokens / 2 * bytesPerToken
	if got := len(s.Compaction.SummaryFallback); got > limit+500 {
		t.Fatalf("fallback is %d bytes; limit %d", got, limit)
	}
}
