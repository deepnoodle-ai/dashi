package dashi_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// hostTurns is a host that keeps its own turns and runs each through a
// Runner, with BuildHistory as the history.
type hostTurns struct {
	t      *testing.T
	client *dashitest.Client
	turns  []dashi.Turn
}

func (h *hostTurns) run(start dashi.Start) {
	h.t.Helper()
	id := fmt.Sprintf("h-%d", len(h.turns)+1)
	start.CommandID = "run:" + id
	start.Agent.Model = dashitest.Model
	out, err := dashi.Runner{Client: h.client}.Run(h.t.Context(), dashi.NewTurn(id), start)
	if err != nil {
		h.t.Fatal(err)
	}
	h.turns = append(h.turns, out.Turn)
}

func (h *hostTurns) send(text, answer string) {
	h.t.Helper()
	h.client.Add(dashitest.Reply(answer))
	history := h.history()
	h.run(dashi.Start{
		History: history,
		Input:   []llm.Message{llm.UserMessage(llm.Text(text))},
	})
}

// compact runs a compaction turn that keeps the last keep messages of the
// history and replies with answer.
func (h *hostTurns) compact(keep int, answer string) {
	h.t.Helper()
	h.client.Add(dashitest.Reply(answer))
	history := h.history()
	h.run(dashi.Start{
		Origin: dashi.StartCompaction,
		Agent:  dashi.Agent{NoTools: true},
		Input:  []llm.Message{llm.UserMessage(llm.Text("Summarize."))},
		Compaction: &dashi.CompactionFact{
			Keep:  slices.Clone(history[len(history)-keep:]),
			Frame: "Summary:",
		},
	})
}

func (h *hostTurns) history() []llm.Message {
	h.t.Helper()
	history, warnings := dashi.BuildHistory(h.turns)
	if len(warnings) > 0 {
		h.t.Fatalf("warnings=%v", warnings)
	}
	return history
}

func TestBuildHistory(t *testing.T) {
	if history, warnings := dashi.BuildHistory(nil); history != nil || warnings != nil {
		t.Fatalf("empty: history=%v warnings=%v", history, warnings)
	}

	h := &hostTurns{t: t, client: dashitest.NewClient()}
	h.send("first", "one")
	h.send("second", "two")
	if got := textsOf(h.history()); !slices.Equal(got,
		[]string{"first", "one", "second", "two"}) {
		t.Fatalf("no compaction: %q", got)
	}

	// A failed compaction replaces nothing.
	h.compact(2, "")
	if got := textsOf(h.history()); !slices.Equal(got,
		[]string{"first", "one", "second", "two"}) {
		t.Fatalf("failed compaction: %q", got)
	}

	// A completed compaction replaces the history before it, and the
	// turns after it follow. The turns before it may be left out.
	h.compact(2, "<summary>S1</summary>")
	h.send("third", "three")
	want := []string{"Summary:\n\nS1", "second", "two", "third", "three"}
	if got := textsOf(h.history()); !slices.Equal(got, want) {
		t.Fatalf("after compaction: %q", got)
	}
	tail, _ := dashi.BuildHistory(h.turns[3:])
	if got := textsOf(tail); !slices.Equal(got, want) {
		t.Fatalf("from the compaction turn: %q", got)
	}

	// A compaction of a compaction starts from the newest one.
	h.compact(2, "<summary>S2</summary>")
	h.send("fourth", "four")
	if got := textsOf(h.history()); !slices.Equal(got,
		[]string{"Summary:\n\nS2", "third", "three", "fourth", "four"}) {
		t.Fatalf("compaction of a compaction: %q", got)
	}
}

func TestBuildHistoryUnreadableTurn(t *testing.T) {
	h := &hostTurns{t: t, client: dashitest.NewClient()}
	h.send("first", "one")
	h.send("second", "two")
	turns := slices.Clone(h.turns)
	turns[0].Steps = turns[0].Steps[1:] // no start fact
	history, warnings := dashi.BuildHistory(turns)
	if got := textsOf(history); !slices.Equal(got, []string{"second", "two"}) {
		t.Fatalf("history=%q", got)
	}
	if got := codes(warnings); !slices.Equal(got,
		[]dashi.WarningCode{dashi.WarningTurnUnreadable}) {
		t.Fatalf("warnings=%v", warnings)
	}
}

func textsOf(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Text()
	}
	return out
}

func codes(ws []dashi.Warning) []dashi.WarningCode {
	var out []dashi.WarningCode
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}
