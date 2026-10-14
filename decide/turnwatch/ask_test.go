package turnwatch

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide"
)

// A turn of text replies has none of its tool calls in the messages, so
// its latest reply is the recent activity.
func TestBuildStateWithoutToolCalls(t *testing.T) {
	p := dashi.ModelPlan{
		Turn: dashi.TurnView{
			Input: []llm.Message{llm.UserMessage(llm.Text("Plan the release."))},
			Reply: "First I will list the open issues.",
		},
		Input: llm.Input{Messages: []llm.Message{
			llm.UserMessage(llm.Text("Plan the release.")),
			llm.AssistantMessage(llm.Text("First I will list the open issues.")),
		}},
	}
	s := buildState(p)
	if len(s.Recent) != 1 || s.Recent[0].AssistantText != "First I will list the open issues." {
		t.Fatalf("recent = %+v", s.Recent)
	}
}

// With nothing to judge, a check_every trigger sends no request. The
// Watcher has no client, so a request would fail the check.
func TestEmptyStateAsksNothing(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	var seen []Check
	w := &Watcher{CheckEvery: 2, OnCheck: func(c Check) { seen = append(seen, c) }}
	p := dashi.ModelPlan{
		Turn: dashi.TurnView{
			ID:         "t1",
			ModelCalls: 2,
			Input:      []llm.Message{llm.UserMessage(llm.Text("Plan the release."))},
		},
		Input: llm.Input{Messages: []llm.Message{llm.UserMessage(llm.Text("Plan the release."))}},
	}
	d, err := w.check(t.Context(), p)
	if err != nil || len(d.AddMessages) != 0 || len(seen) != 0 {
		t.Fatalf("decision %+v, err %v, checks %+v", d, err, seen)
	}
}

func TestYesProbabilitiesRejectsInvalid(t *testing.T) {
	for _, p := range []float64{math.NaN(), -0.1, 1.5} {
		resp := &decide.Response{Answers: map[string]decide.Answer{
			QuestionRepeating:  &decide.NoulAnswer{Noul: p},
			QuestionOffRequest: &decide.NoulAnswer{Noul: 0.2},
		}}
		if _, err := yesProbabilities(resp); err == nil {
			t.Errorf("P(yes) %v was accepted", p)
		}
	}
}

func TestCutMiddle(t *testing.T) {
	if got := cutMiddle("short", 10); got != "short" {
		t.Fatalf("cutMiddle kept %q", got)
	}
	text := "--- FAIL: TestParse\n" + strings.Repeat("log line\n", 100) +
		"parser_test.go:41: got 4 tokens, want 3"
	got := cutMiddle(text, 100)
	if !strings.HasPrefix(got, "--- FAIL: TestParse") ||
		!strings.HasSuffix(got, "got 4 tokens, want 3") ||
		!strings.Contains(got, elided) || len(got) > 100+len(elided) {
		t.Fatalf("cutMiddle = %q", got)
	}
	// Both cuts end on whole characters.
	wide := strings.Repeat("é", 100)
	if got := cutMiddle(wide, 51); !utf8.ValidString(got) {
		t.Fatalf("cutMiddle split a character: %q", got)
	}
}
