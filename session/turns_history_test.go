package session_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// BuildHistory over a session's turns gives the session's projected
// history, and checks the digest the session recorded.
func TestBuildHistoryMatchesSession(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		used("two", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("three"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100, keep: 1}
	sendAll(t, s, "first", "second", "third")
	want, err := s.NextHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var turns []dashi.Turn
	for i := 1; i <= 4; i++ {
		turns = append(turns, loadTurn(t, rec, session.TurnID("s", i)))
	}
	got, warnings := dashi.BuildHistory(turns)
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) || len(warnings) != 0 {
		t.Fatalf("got %s, want %s; warnings=%v", a, b, warnings)
	}

	turns[0].Steps[0].Started.Input = []llm.Message{llm.UserMessage(llm.Text("changed"))}
	got, warnings = dashi.BuildHistory(turns)
	if a, _ := json.Marshal(got); string(a) != string(b) {
		t.Fatalf("a mismatch changed the history: %s", a)
	}
	if got := codes(warnings); !slices.Equal(got,
		[]dashi.WarningCode{dashi.WarningCompactionMismatch}) {
		t.Fatalf("warnings=%v", warnings)
	}
}

// A Session fills the digest a custom Compactor leaves empty, from the
// history it compacted, so the check still applies.
func TestSessionFillsEmptyCompactionHash(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		used("two", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("three"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100, keep: 1} // leaves Digest empty
	sendAll(t, s, "first", "second")
	before, err := s.NextHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, err := dashi.HistoryHash(before)
	if err != nil {
		t.Fatal(err)
	}
	sendAll(t, s, "third")
	if got := load(t, rec, "s-3").Compaction.Hash; got != want {
		t.Fatalf("digest=%q, want %q", got, want)
	}
}
