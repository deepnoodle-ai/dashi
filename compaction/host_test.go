package compaction_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A host's compaction turn carries the Compactor's hash, so
// BuildHistory checks it as it checks a Session's: an altered earlier
// turn warns, and the replacement history is still used.
func TestHostCompactionHash(t *testing.T) {
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		dashitest.Reply("two"),
		dashitest.Reply("<summary>S</summary>"),
	)
	runner := dashi.Runner{Client: client}
	var turns []dashi.Turn
	run := func(start dashi.Start) {
		t.Helper()
		id := fmt.Sprintf("h-%d", len(turns)+1)
		start.CommandID = "run:" + id
		start.Agent.Model = dashitest.Model
		out, err := runner.Run(t.Context(), dashi.NewTurn(id), start)
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, out.Turn)
	}
	for _, text := range []string{"first", "second"} {
		history, _ := dashi.BuildHistory(turns)
		run(dashi.Start{
			History: history,
			Input:   []llm.Message{llm.UserMessage(llm.Text(text))},
		})
	}
	history, _ := dashi.BuildHistory(turns)
	start, err := (&compaction.Compactor{}).Start(history)
	if err != nil {
		t.Fatal(err)
	}
	if start.Compaction.Hash == "" {
		t.Fatal("the compaction fact has no digest")
	}
	run(start)

	want, warnings := dashi.BuildHistory(turns)
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v", warnings)
	}
	turns[0].Steps[0].Started.Input = []llm.Message{llm.UserMessage(llm.Text("changed"))}
	got, warnings := dashi.BuildHistory(turns)
	if len(warnings) != 1 || warnings[0].Code != dashi.WarningCompactionMismatch {
		t.Fatalf("warnings=%v", warnings)
	}
	text := func(ms []llm.Message) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Text())
		}
		return out
	}
	if !slices.Equal(text(got), text(want)) {
		t.Fatalf("history=%q, want the replacement %q", text(got), text(want))
	}
}
