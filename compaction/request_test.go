package compaction

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
)

// dueAtWindow is a policy whose due count is its whole window, so the room
// for the summary is the window.
func dueAtWindow(window int) Policy {
	p := policyWith(window)
	p.DueTokens = window
	return p
}

func TestStartShortensOversizedMessagesWithoutDroppingAny(t *testing.T) {
	c := &Compactor{Policy: dueAtWindow(8_000), MaxTokens: 1_000, TailFraction: -1}
	s, err := c.Start(t.Context(), exchange(0, 300_000), llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	req := s.Input[0].Text()
	// Budget: (8000-1000) * 0.75 * 4 bytes, plus the prompt and markers.
	if len(req) > 21_000+len(DefaultPrompt)+2_000 {
		t.Fatalf("request is %d bytes", len(req))
	}
	for _, want := range []string{
		"prompt 0",
		"[tool result id=call-0-1]",
		"[tool result id=call-0-2]",
		"bytes cut",
	} {
		if !strings.Contains(req, want) {
			t.Fatalf("request lacks %q", want)
		}
	}
	if s.Agent.Settings.MaxTokens != 1_000 {
		t.Fatalf("max tokens = %d", s.Agent.Settings.MaxTokens)
	}
}

// Failure 2: when even 1 KB per item does not fit, Start drops whole items
// from the middle, keeping the oldest (the task) and the newest, with one
// marker. It never fails on size.
func TestStartDropsMiddleItemsWhenHistoryCannotFit(t *testing.T) {
	history, _ := conversation(50, 3_000)
	c := &Compactor{Policy: dueAtWindow(2_000), MaxTokens: 500, TailFraction: -1}
	s, err := c.Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	req := s.Input[0].Text()
	for _, want := range []string{"prompt 0", "items omitted]", "answer 49"} {
		if !strings.Contains(req, want) {
			t.Fatalf("request lacks %q:\n%s", want, req)
		}
	}
	if strings.Count(req, "items omitted]") != 1 {
		t.Fatal("more than one omission marker")
	}
	// The budget is (2000-500) * 0.75 * 4 bytes, plus the prompt and markers.
	if len(req) > 4_500+len(DefaultPrompt)+1_000 {
		t.Fatalf("request is %d bytes", len(req))
	}
}

func TestFitTranscriptKeepsCountAndBudget(t *testing.T) {
	items := []string{
		strings.Repeat("a", 50_000),
		"short",
		strings.Repeat("é", 20_000),
		strings.Repeat("b", 30_000),
	}
	got := shortenTranscript(items, 20_000)
	if len(got) != len(items) || got[1] != "short" {
		t.Fatalf("items changed shape: %d", len(got))
	}
	total := 0
	for _, s := range got {
		total += len(s)
		if !utf8.ValidString(s) {
			t.Fatal("cut split a rune")
		}
	}
	if total > 20_000+3*40 {
		t.Fatalf("total = %d", total)
	}
}

func TestStartSmallWindowShrinksOutputAllowance(t *testing.T) {
	history, _ := conversation(2, 200)
	// The default allowance (16,384) is larger than the whole window.
	s, err := (&Compactor{Policy: dueAtWindow(8_000)}).Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Agent.Settings.MaxTokens; got != 2_000 {
		t.Fatalf("max tokens = %d, want a quarter of the window", got)
	}
}
