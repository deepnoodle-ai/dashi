package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func compact(
	t *testing.T,
	s Summarizer,
	history []llm.Message,
	prev *Record,
	opts Options,
) *Record {
	t.Helper()
	rec, err := Compact(context.Background(), s, history, prev, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCompactKeepsToolPairsValidAtEveryTurnStart(t *testing.T) {
	history, starts := session(6, 400)
	// Each fraction moves the tail budget across a different Turn start.
	for _, fraction := range []float64{0.02, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.7} {
		s := &fakeSummarizer{text: "short"}
		rec := compact(t, s, history, nil, Options{
			Policy:       policyWith(20_000),
			TurnStarts:   starts,
			TailFraction: fraction,
		})
		view, err := Project(history, rec)
		if err != nil {
			t.Fatal(err)
		}
		validPairs(t, view)
		onStart := rec.Covers.Messages == len(history)
		for _, s := range starts {
			onStart = onStart || rec.Covers.Messages == s
		}
		if !onStart {
			t.Fatalf("cut %d is not a turn start", rec.Covers.Messages)
		}
	}
}

func TestCompactSkipsCutThatSplitsAnExchange(t *testing.T) {
	history, starts := session(4, 400)
	// Index 2 is the tool-result message of Turn 0, so a cut there would
	// leave results without their calls. The host gave a wrong start.
	bad := []int{starts[0], 2, starts[2], starts[3]}
	s := &fakeSummarizer{text: "short"}
	rec := compact(t, s, history, nil, Options{
		Policy:       policyWith(20_000),
		TurnStarts:   bad,
		TailFraction: 0.9,
	})
	if rec.Covers.Messages == 2 {
		t.Fatal("cut split a tool exchange")
	}
	view, _ := Project(history, rec)
	validPairs(t, view)
}

func TestCompactNeverCutsAtASteer(t *testing.T) {
	history, starts := session(3, 400)
	steer := llm.UserMessage(llm.Text("use the staging database"))
	// Insert a steer inside Turn 1, after its first assistant message.
	at := starts[1] + 4
	rest := append([]llm.Message{steer}, history[at:]...)
	history = append(history[:at:at], rest...)
	starts[2]++
	for _, fraction := range []float64{0.05, 0.2, 0.5} {
		s := &fakeSummarizer{text: "short"}
		rec := compact(t, s, history, nil, Options{
			Policy:       policyWith(20_000),
			TurnStarts:   starts,
			TailFraction: fraction,
		})
		if rec.Covers.Messages == at {
			t.Fatal("cut fell on a steer inside a Turn")
		}
	}
}

func TestCompactKeepsTailWithinBudget(t *testing.T) {
	history, starts := session(4, 2_000)
	s := &fakeSummarizer{text: "short summary"}
	rec := compact(t, s, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
	})
	view, err := Project(history, rec)
	if err != nil {
		t.Fatal(err)
	}
	before, after := 0, 0
	for _, m := range history {
		before += estimate(m)
	}
	for _, m := range view {
		after += estimate(m)
	}
	if after*2 > before {
		t.Fatalf("view shrank from %d to %d, want at least half", before, after)
	}
	if rec.Covers.Turns != 3 || rec.Covers.Messages != starts[3] {
		t.Fatalf("covers %+v, want 3 Turns and the last Turn kept", rec.Covers)
	}
	if rec.TokensAfter >= rec.TokensBefore {
		t.Fatalf(
			"tokens %d -> %d did not shrink",
			rec.TokensBefore,
			rec.TokensAfter,
		)
	}
}

func TestCompactSummarizesAllWhenNewestTurnIsTooLarge(t *testing.T) {
	small, starts := session(3, 100)
	huge := exchange(3, 60_000)
	history := append(small, huge...)
	starts = append(starts, len(small))
	s := &fakeSummarizer{text: "short"}
	rec := compact(t, s, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
	})
	if rec.Covers.Messages != len(history) || rec.Covers.Turns != 4 {
		t.Fatalf("covers %+v, want all history", rec.Covers)
	}
	view, _ := Project(history, rec)
	if len(view) != 1 {
		t.Fatalf("view has %d messages, want the summary alone", len(view))
	}
}

func TestCompactWithFewTurnsMakesNoCall(t *testing.T) {
	for _, turns := range []int{0, 1} {
		history, starts := session(turns, 100)
		s := &fakeSummarizer{text: "short"}
		_, err := Compact(context.Background(), s, history, nil, Options{
			TurnStarts: starts,
		})
		if !errors.Is(err, ErrNothingToCompact) || s.calls != 0 {
			t.Fatalf("turns=%d: err=%v calls=%d", turns, err, s.calls)
		}
	}
}

func TestCompactLeavesNoRecordOnFailure(t *testing.T) {
	history, starts := session(4, 2_000)
	opts := Options{Policy: policyWith(10_000), TurnStarts: starts}
	cases := []struct {
		name string
		s    *fakeSummarizer
		want error
	}{
		{"provider error", &fakeSummarizer{err: errBoom}, errBoom},
		{"empty summary", &fakeSummarizer{text: "  \n"}, nil},
		{
			"no savings",
			&fakeSummarizer{text: strings.Repeat("y", 100_000)},
			ErrNoSavings,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, _ := json.Marshal(history)
			rec, err := Compact(context.Background(), c.s, history, nil, opts)
			if rec != nil || err == nil {
				t.Fatalf("rec=%v err=%v, want an error and no record", rec, err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			after, _ := json.Marshal(history)
			if string(before) != string(after) {
				t.Fatal("history changed")
			}
		})
	}
}

func TestCompactNormalizesBadTurnStarts(t *testing.T) {
	history, good := session(4, 400)
	n := len(history)
	cases := map[string][]int{
		"repeat":       {good[0], good[1], good[1], good[2], good[3]},
		"empty turn":   append(append([]int{}, good...), n),
		"out of order": {good[0], good[2], good[1], good[3]},
		"negative":     append([]int{-1}, good...),
		"far":          append(append([]int{}, good...), n+50),
	}
	for name, starts := range cases {
		t.Run(name, func(t *testing.T) {
			rec := compact(t, &fakeSummarizer{text: "short"}, history, nil,
				Options{Policy: policyWith(20_000), TurnStarts: starts})
			if len(rec.Warnings) == 0 {
				t.Fatal("no warning for changed starts")
			}
			if rec.Covers.Turns < 1 || rec.Covers.Turns > 4 || rec.Covers.Messages != coverEnd(good, len(history), rec.Covers.Turns) {
				t.Fatalf("coverage = %+v", rec.Covers)
			}
			view, err := Project(history, rec)
			if err != nil {
				t.Fatal(err)
			}
			validPairs(t, view)
		})
	}
}

func TestCompactCleanStartsGiveNoWarning(t *testing.T) {
	history, starts := session(4, 400)
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil,
		Options{Policy: policyWith(20_000), TurnStarts: starts})
	if len(rec.Warnings) != 0 {
		t.Fatalf("warnings = %v", rec.Warnings)
	}
}

func TestCompactNegativeTailKeepsNoTurns(t *testing.T) {
	history, starts := session(4, 400)
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil,
		Options{Policy: policyWith(20_000), TurnStarts: starts, TailFraction: -1})
	if rec.Covers.Messages != len(history) {
		t.Fatalf("covers %d of %d", rec.Covers.Messages, len(history))
	}
}

func TestCompactRemovesRemindersFromSummarizerInput(t *testing.T) {
	history, starts := session(4, 2_000)
	reminder := llm.ReminderMessage(llm.Reminder{
		Name:    "git-status",
		Tier:    llm.ReminderContextual,
		Content: "SECRET-REMINDER",
	})
	history[0].Content = append(history[0].Content, reminder.Content...)
	rest := append([]llm.Message{reminder}, history[1:]...)
	history = append(history[:1:1], rest...)
	for i := 1; i < len(starts); i++ {
		starts[i]++
	}
	s := &fakeSummarizer{text: "short"}
	compact(t, s, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
	})
	data, _ := json.Marshal(s.input)
	if strings.Contains(string(data), "SECRET-REMINDER") {
		t.Fatal("reminder reached the summarizer")
	}
	if len(s.input) == 0 {
		t.Fatal("summarizer got no messages")
	}
}

func TestCompactRecordsSummarizerSpend(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
	})
	if rec.Model != "summary-model" || rec.Usage.OutputTokens != 5 {
		t.Fatalf("model=%q usage=%+v", rec.Model, rec.Usage)
	}
	if rec.Cost["USD"].String() != "0.02" {
		t.Fatalf("cost = %v", rec.Cost)
	}
	if rec.Version != 1 || rec.CreatedAt.IsZero() {
		t.Fatalf("version=%d created=%v", rec.Version, rec.CreatedAt)
	}
}

func TestCompactTokensBeforeUsesProviderUsage(t *testing.T) {
	history, starts := session(4, 2_000)
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil, Options{
		Policy:     policyWith(10_000),
		TurnStarts: starts,
		Last: llm.Usage{
			InputTokens:          100,
			CacheReadInputTokens: 8_000,
			OutputTokens:         50,
		},
	})
	if rec.TokensBefore != 8_150 {
		t.Fatalf("TokensBefore = %d, want 8150", rec.TokensBefore)
	}
}

func TestCompactChainsFromPreviousSummary(t *testing.T) {
	history, starts := session(6, 2_000)
	opts := Options{Policy: policyWith(10_000), TurnStarts: starts[:4]}
	old := history[:starts[4]]
	first := compact(t, &fakeSummarizer{text: "first"}, old, nil, opts)

	s := &fakeSummarizer{text: "second"}
	opts.TurnStarts = starts
	second := compact(t, s, history, first, opts)
	if second.Covers.Messages <= first.Covers.Messages {
		t.Fatalf(
			"second covers %d, first %d",
			second.Covers.Messages,
			first.Covers.Messages,
		)
	}
	if !strings.Contains(s.input[0].Text(), "first") {
		t.Fatal("summarizer did not get the previous summary first")
	}
	newer := history[first.Covers.Messages:second.Covers.Messages]
	if want := len(newer) + 1; len(s.input) != want {
		t.Fatalf("summarizer got %d messages, want %d", len(s.input), want)
	}
	view, err := Project(history, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != len(history)-second.Covers.Messages+1 {
		t.Fatalf("view has %d messages", len(view))
	}
	if !strings.Contains(view[0].Text(), "second") {
		t.Fatal("view does not start with the newest summary")
	}
}

func TestCompactRefusesRecordThatDoesNotMatch(t *testing.T) {
	history, starts := session(4, 2_000)
	opts := Options{Policy: policyWith(10_000), TurnStarts: starts}
	rec := compact(t, &fakeSummarizer{text: "short"}, history, nil, opts)
	history[0] = llm.UserMessage(llm.Text("changed"))
	s := &fakeSummarizer{text: "x"}
	_, err := Compact(context.Background(), s, history, rec, opts)
	if !errors.Is(err, ErrCoverageMismatch) || s.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, s.calls)
	}
}

// coverEnd returns the history index where the Turn after n covered Turns
// starts, or the history length when all Turns are covered.
func coverEnd(starts []int, total, n int) int {
	if n >= len(starts) {
		return total
	}
	return starts[n]
}
