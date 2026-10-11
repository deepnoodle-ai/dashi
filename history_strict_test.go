package dashi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func strictSummary(h *hostTurns, text string) {
	h.t.Helper()
	hash, err := dashi.HistoryHash(h.history())
	if err != nil {
		h.t.Fatal(err)
	}
	h.client.Add(dashitest.Reply(text))
	h.run(dashi.Start{
		Origin:     dashi.StartCompaction,
		Agent:      dashi.Agent{NoTools: true},
		Input:      []llm.Message{llm.UserMessage(llm.Text("Summarize."))},
		Compaction: &dashi.CompactionFact{Hash: hash, Frame: "Summary:"},
	})
}

func TestBuildHistoryStrictChecksHiddenRecordsAndHashes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]dashi.Turn)
		turnID string
		cause  error
	}{
		{
			"hidden ordinary",
			func(ts []dashi.Turn) { ts[0].Steps[0].Seq++ },
			"h-1",
			dashi.ErrInvalidTurn,
		},
		{
			"hidden summary",
			func(ts []dashi.Turn) { ts[1].Steps[0].Seq++ },
			"h-2",
			dashi.ErrInvalidTurn,
		},
		{
			"nested mismatch",
			func(ts []dashi.Turn) { ts[1].Steps[0].Started.Compaction.Hash = "wrong" },
			"h-2",
			dashi.ErrCompactionMismatch,
		},
		{
			"latest mismatch",
			func(ts []dashi.Turn) { ts[3].Steps[0].Started.Compaction.Hash = "wrong" },
			"h-4",
			dashi.ErrCompactionMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &hostTurns{t: t, client: dashitest.NewClient()}
			h.send("first", "one")
			strictSummary(h, "S1")
			h.send("second", "two")
			strictSummary(h, "S2")
			tc.change(h.turns)
			before, _ := json.Marshal(h.turns)
			history, err := dashi.BuildHistoryStrict(h.turns)
			if history != nil {
				t.Fatal("invalid records returned usable history")
			}
			var problem *dashi.HistoryError
			if !errors.As(err, &problem) || problem.TurnID != tc.turnID {
				t.Fatalf("error does not identify %s: %v", tc.turnID, err)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("error lost cause %v: %v", tc.cause, err)
			}
			after, _ := json.Marshal(h.turns)
			if string(before) != string(after) {
				t.Fatal("construction changed supplied records")
			}
		})
	}
}

func TestBuildHistoryStrictPreservesValidHistoryRules(t *testing.T) {
	for _, turns := range [][]dashi.Turn{nil, {}} {
		history, err := dashi.BuildHistoryStrict(turns)
		if err != nil || history != nil {
			t.Fatalf("empty input: history=%v error=%v", history, err)
		}
	}
	h := &hostTurns{t: t, client: dashitest.NewClient()}
	h.send("first", "one")
	h.compact(1, "")   // an empty summary replaces nothing
	h.compact(1, "S1") // historical valid metadata has no source hash
	h.send("second", "two")
	strictSummary(h, "S2")
	h.send("third", "three")
	h.turns[0].Steps[0].Warnings = []dashi.Warning{{
		Code:    dashi.WarningHooksChanged,
		Message: "hooks changed",
	}}
	for _, supplied := range [][]dashi.Turn{h.turns, h.turns[4:]} {
		want, warnings := dashi.BuildHistory(supplied)
		if len(warnings) != 0 {
			t.Fatalf("fixture has integrity warnings: %v", warnings)
		}
		got, err := dashi.BuildHistoryStrict(supplied)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("strict history differs: %v, %v", got, err)
		}
	}
}

func TestBuildHistoryStrictRejectsUnresolvedOrdinaryMessages(t *testing.T) {
	client := dashitest.NewClient(dashitest.Reply("one"))
	out, err := (dashi.Runner{Client: client}).Run(
		t.Context(), dashi.NewTurn("pending"),
		dashi.Start{
			Agent: dashi.Agent{Model: dashitest.Model},
			Input: []llm.Message{llm.UserMessage(llm.Text("work"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallStarted {
			out.Turn.Steps = out.Turn.Steps[:i+1]
			break
		}
	}
	if _, err := out.Turn.View(); err != nil {
		t.Fatal(err)
	}
	history, err := dashi.BuildHistoryStrict([]dashi.Turn{out.Turn})
	if history != nil || err == nil {
		t.Fatalf("unresolved ordinary work returned history: %v, %v", history, err)
	}
}

func ExampleBuildHistoryStrict() {
	broken := dashi.Turn{
		ID: "support-7",
		Steps: []dashi.Step{{
			Seq: 2, Kind: dashi.StepTurnStarted,
		}},
	}
	history, err := dashi.BuildHistoryStrict([]dashi.Turn{broken})
	var problem *dashi.HistoryError
	if errors.As(err, &problem) {
		fmt.Println("repair history turn:", problem.TurnID)
	}
	fmt.Println("history withheld:", history == nil)
	// Output:
	// repair history turn: support-7
	// history withheld: true
}
