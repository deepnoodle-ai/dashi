package compaction

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// replaced is the replacement history a compaction turn with the reply
// answer would record.
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
	history, starts := conversation(6, 400)
	for _, fraction := range []float64{0.02, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.7} {
		c := &Compactor{Policy: policyWith(20_000), TailFraction: fraction}
		s, err := c.Start(t.Context(), history, llm.Usage{})
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
	s, err := c.Start(t.Context(), history, llm.Usage{})
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

// Agent mail is a reminder with a sender. Nobody supplies it again, so the
// summary request carries it; a host reminder stays out.
func TestCompactorSummaryRequestKeepsAgentMail(t *testing.T) {
	history := exchange(0, 100)
	history = append(history,
		llm.ReminderMessage(llm.Reminder{
			Name:    "git-status",
			Tier:    llm.ReminderContextual,
			Content: "SECRET-REMINDER",
		}),
		llm.ReminderMessage(llm.Reminder{
			Name:    "task-ended",
			Tier:    llm.ReminderContextual,
			From:    "task t_k3f2 (explore)",
			Content: "Priya owns the billing service.",
		}),
	)
	history = append(history, exchange(1, 100)...)
	c := &Compactor{Policy: policyWith(100_000), TailFraction: -1}
	s, err := c.Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	request := s.Input[0].Text()
	want := "[message from task t_k3f2 (explore)]\nPriya owns the billing service."
	if !strings.Contains(request, want) {
		t.Fatalf("request lacks the agent mail:\n%s", request)
	}
	if strings.Contains(request, "SECRET-REMINDER") {
		t.Fatal("a host reminder reached the summary request")
	}
}

func TestCompactorNeeded(t *testing.T) {
	c := New(policyWith(100_000))        // due at 20,000 tokens
	history, _ := conversation(4, 5_000) // about 15,000 tokens
	if !c.Needed(history, llm.Usage{InputTokens: 20_000}) {
		t.Fatal("not needed at the due count")
	}
	if c.Needed(history, llm.Usage{InputTokens: 19_999}) {
		t.Fatal("needed below the due count")
	}
	only := []llm.Message{system("Be terse.")}
	if c.Needed(only, llm.Usage{InputTokens: 90_000}) {
		t.Fatal("needed with nothing to summarize")
	}
	if _, err := c.Start(t.Context(), only, llm.Usage{}); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("err=%v", err)
	}
}

// Failure 3: when the fixed part (what the last call sent besides the
// history, such as tool definitions, or system messages) fills the window,
// no replacement can be below the due count: it is not needed, and Start
// makes no useless summary call. Below that, a fixed part over half the due
// count raises the due count instead of turning compaction off.
func TestCompactorFixedPart(t *testing.T) {
	c := New(policyWith(100_000)) // due at 20,000 tokens
	history, _ := conversation(4, 5_000)
	// About 42,000 tokens of tools raise the due count to about 52,000.
	short, _ := conversation(2, 5_000) // about 8,000 tokens
	if c.Needed(short, llm.Usage{InputTokens: 50_000}) {
		t.Fatal("needed below the raised due count")
	}
	if !c.Needed(history, llm.Usage{InputTokens: 60_000}) {
		t.Fatal("large tools turned compaction off")
	}
	if c.Needed(history, llm.Usage{InputTokens: 120_000}) {
		t.Fatal("needed although the tools alone fill the window")
	}
	if _, err := c.Start(t.Context(), history, llm.Usage{InputTokens: 120_000}); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("Start with no room: %v", err)
	}
	big := []llm.Message{system(strings.Repeat("rule ", 90_000))}
	big = append(big, history...)
	if c.Needed(big, llm.Usage{InputTokens: 140_000}) {
		t.Fatal("needed although the system messages fill the window")
	}
}

// Needed estimates tokens from history size, so tool output added after the
// last model call counts. 2,000 tokens is about 8,000 bytes of history.
func TestCompactorNeededHistorySize(t *testing.T) {
	c := New(policyWith(10_000))
	sized := func(bytes int) []llm.Message {
		m := []llm.Message{llm.UserMessage(llm.Text(""))}
		b, _ := json.Marshal(m)
		return []llm.Message{llm.UserMessage(llm.Text(strings.Repeat("x", bytes-len(b))))}
	}
	if c.Needed(sized(7_996), llm.Usage{}) {
		t.Fatal("needed just under the due count")
	}
	if !c.Needed(sized(8_000), llm.Usage{}) {
		t.Fatal("not needed at the due count")
	}
}

// DueTokens sets the due count; the tail and the allowance are shares of the
// room below it.
func TestCompactorSizesFromDueTokens(t *testing.T) {
	p := policyWith(1_000_000)
	p.DueTokens = 40_000
	history, starts := conversation(8, 4_000) // about 24,000 tokens
	c := New(p)
	if !c.Needed(history, llm.Usage{InputTokens: 40_000}) {
		t.Fatal("not needed at DueTokens")
	}
	s, err := c.Start(t.Context(), history, llm.Usage{InputTokens: 40_000})
	if err != nil {
		t.Fatal(err)
	}
	// Room: 40,000 less about 16,000 of tools is about 24,000 tokens.
	if got := s.Agent.Settings.MaxTokens; got > 24_000/4+500 || got < 24_000/4-1_000 {
		t.Fatalf("allowance = %d, want a quarter of the room", got)
	}
	keep := s.Compaction.Keep
	if len(keep) == 0 || len(keep) > len(history)/2 ||
		!sameJSON(t, keep, history[len(history)-len(keep):]) || len(keep) < len(history)-starts[7] {
		t.Fatalf("kept %d messages, want the newest turns", len(keep))
	}
}

func system(text string) llm.Message {
	return llm.Message{Role: llm.RoleSystem, Content: []llm.Content{llm.Text(text)}}
}

// A summary model with a smaller window than the session's sizes the
// transcript and the output allowance to its own window.
func TestCompactorSummaryModelWindow(t *testing.T) {
	history, _ := conversation(8, 400_000) // about 800k tokens
	session := &Compactor{Policy: policyWith(1_000_000), TailFraction: -1}
	small := *session
	small.Model = "claude-haiku-4-5" // a 200k-token window in the catalog
	big, err := session.Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := small.Start(t.Context(), history, llm.Usage{})
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

func TestCompactorKeepsTailWithinBudget(t *testing.T) {
	history, starts := conversation(4, 2_000)
	s, err := (&Compactor{Policy: policyWith(40_000)}).Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, s.Compaction.Keep, history[starts[3]:]) {
		t.Fatalf("kept %d messages, want the last turn", len(s.Compaction.Keep))
	}
	if s.Origin != dashi.StartCompaction {
		t.Fatalf("origin = %q", s.Origin)
	}
}

func TestCompactorSummarizesAllWhenNewestTurnIsTooLarge(t *testing.T) {
	history, _ := conversation(3, 100)
	history = append(history, exchange(3, 60_000)...)
	s, err := (&Compactor{Policy: policyWith(10_000)}).Start(t.Context(), history, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Compaction.Keep) != 0 {
		t.Fatalf("kept %d messages, want none", len(s.Compaction.Keep))
	}
}
