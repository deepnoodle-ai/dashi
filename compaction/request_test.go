package compaction

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStartShortensOversizedMessagesWithoutDroppingAny(t *testing.T) {
	c := &Compactor{Policy: policyWith(8_000), MaxTokens: 1_000, TailFraction: -1}
	s, err := c.Start(t.Context(), exchange(0, 300_000))
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

func TestStartFailsWhenHistoryCannotFit(t *testing.T) {
	history, _ := conversation(50, 3_000)
	c := &Compactor{Policy: policyWith(2_000), MaxTokens: 500, TailFraction: -1}
	if _, err := c.Start(t.Context(), history); err == nil {
		t.Fatal("history larger than the window was sent")
	}
}

func TestFitTranscriptKeepsCountAndBudget(t *testing.T) {
	items := []string{
		strings.Repeat("a", 50_000),
		"short",
		strings.Repeat("é", 20_000),
		strings.Repeat("b", 30_000),
	}
	got, err := fitTranscript(items, 20_000)
	if err != nil {
		t.Fatal(err)
	}
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
	s, err := (&Compactor{Policy: policyWith(8_000)}).Start(t.Context(), history)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Agent.Settings.MaxTokens; got != 2_000 {
		t.Fatalf("max tokens = %d, want a quarter of the window", got)
	}
}
