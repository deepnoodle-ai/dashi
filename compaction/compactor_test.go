package compaction

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// replaced is the replacement history a compaction turn with answer would
// record.
func replaced(t *testing.T, s dashi.Start, answer string) []llm.Message {
	t.Helper()
	f := s.Compaction
	if f == nil || f.SummaryAt < 0 || f.SummaryAt > len(f.Keep) {
		t.Fatalf("compaction fact %+v", f)
	}
	out := slices.Clone(f.Keep[:f.SummaryAt])
	out = append(out, llm.UserMessage(llm.Text(f.Frame+"\n\n"+answer)))
	return append(out, f.Keep[f.SummaryAt:]...)
}

func TestCompactorKeepsToolPairsValidAtEveryCut(t *testing.T) {
	history, starts := session(6, 400)
	for _, fraction := range []float64{0.02, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.7} {
		c := &Compactor{Policy: policyWith(20_000), TailFraction: fraction}
		s, err := c.Start(history)
		if err != nil {
			t.Fatal(err)
		}
		validPairs(t, replaced(t, s, "summary"))
		cut := len(history) - len(s.Compaction.Keep)
		if cut != len(history) && !slices.Contains(starts, cut) {
			t.Fatalf("cut %d is not a turn start", cut)
		}
		if cut == 0 {
			t.Fatal("the cut covers nothing")
		}
	}
}

func TestCompactorStartIsASummaryRequest(t *testing.T) {
	history := append([]llm.Message{
		system("Be terse."),
	}, exchange(0, 100)...)
	history = append(history, exchange(1, 100)...)
	c := &Compactor{Policy: policyWith(100_000), TailFraction: -1, Model: "m"}
	s, err := c.Start(history)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Agent.NoTools || s.Agent.Model != "m" || s.Agent.Settings.MaxTokens <= 0 {
		t.Fatalf("agent=%+v", s.Agent)
	}
	if len(s.Input) != 1 || s.Input[0].Role != llm.RoleUser {
		t.Fatalf("input=%+v", s.Input)
	}
	request := s.Input[0].Text()
	for _, want := range []string{"prompt 0", "answer 1", "[tool result id=call-1-1]", DefaultPrompt} {
		if !strings.Contains(request, want) {
			t.Fatalf("request lacks %q", want)
		}
	}
	if strings.Contains(request, "Be terse.") {
		t.Fatal("the transcript holds the system message")
	}
	// A negative tail keeps none but the system message, ahead of the
	// summary.
	f := s.Compaction
	if len(f.Keep) != 1 || f.SummaryAt != 1 || f.Keep[0].Text() != "Be terse." ||
		f.Frame != summaryFrame {
		t.Fatalf("fact=%+v", f)
	}
}

func TestCompactorNeeded(t *testing.T) {
	c := New(policyWith(1_000))
	history, _ := session(2, 10)
	if !c.Needed(history, llm.Usage{InputTokens: 800}) {
		t.Fatal("not needed at 80%")
	}
	if c.Needed(history, llm.Usage{InputTokens: 799}) {
		t.Fatal("needed below 80%")
	}
	only := []llm.Message{system("Be terse.")}
	if c.Needed(only, llm.Usage{InputTokens: 900}) {
		t.Fatal("needed with nothing to summarize")
	}
	if _, err := c.Start(only); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("err=%v", err)
	}
}

func system(text string) llm.Message {
	return llm.Message{Role: llm.RoleSystem, Content: []llm.Content{llm.Text(text)}}
}

// A summary model with a smaller window than the session's sizes the
// transcript and the output allowance to its own window.
func TestCompactorSummaryModelWindow(t *testing.T) {
	history, _ := session(8, 400_000) // about 800k tokens
	session := &Compactor{Policy: policyWith(1_000_000), TailFraction: -1}
	small := *session
	small.Model = "claude-haiku-4-5" // a 200k-token window in the catalog
	big, err := session.Start(history)
	if err != nil {
		t.Fatal(err)
	}
	s, err := small.Start(history)
	if err != nil {
		t.Fatal(err)
	}
	fits := func(s dashi.Start, window int) bool {
		in := len(s.Input[0].Text()) / bytesPerToken
		return in+s.Agent.Settings.MaxTokens <= window
	}
	if !fits(s, 200_000) || fits(big, 200_000) {
		t.Fatalf("summary request of %d bytes; session model's %d",
			len(s.Input[0].Text()), len(big.Input[0].Text()))
	}
	if got := s.Agent.Settings.MaxTokens; got != defaultSummaryTokens {
		t.Fatalf("max tokens=%d", got)
	}
}
