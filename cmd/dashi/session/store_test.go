package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// echoClient answers every call with the same text.
type echoClient struct{}

func (echoClient) Provider() string { return "test" }

func (echoClient) API() string { return "test.chat" }

func (echoClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (echoClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{"ok":true}`),
	})
}

func (echoClient) Send(context.Context, llm.Call) (llm.Result, error) {
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("answer")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}

func newStore(t *testing.T) *session.Store {
	t.Helper()
	st := session.NewStore(t.TempDir())
	st.DisableSync()
	return st
}

// addTurn runs one turn of s and records its ID.
func addTurn(
	t *testing.T,
	st *session.Store,
	s *session.Session,
	rec dashi.Recorder,
	text string,
) {
	t.Helper()
	hist, err := st.History(t.Context(), rec.(dashi.Loader), s)
	if err != nil {
		t.Fatal(err)
	}
	id := s.NextTurnID()
	runner := dashi.Runner{
		Agent:    dashi.Agent{Model: "fixture"},
		Client:   echoClient{},
		Recorder: rec,
	}
	out, err := runner.Run(t.Context(), dashi.NewTurn(id), dashi.Start{
		Input:   []llm.Message{llm.UserMessage(llm.Text(text))},
		History: hist.Messages,
	})
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	s.Turns = append(s.Turns, id)
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
}

func TestCreateListLatestByDirectory(t *testing.T) {
	st := newStore(t)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	tick := 0
	st = session.WithClock(st, func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	})
	create := func(dir string) *session.Session {
		s, err := st.Create(dir, "m")
		if err != nil {
			t.Fatal(err)
		}
		rec, err := st.Recorder(s)
		if err != nil {
			t.Fatal(err)
		}
		addTurn(t, st, s, rec, "hello")
		return s
	}
	a, b, a2 := create("/repo/a"), create("/repo/b"), create("/repo/a")
	list, err := st.List("/repo/a")
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].ID != a2.ID || list[1].ID != a.ID {
		t.Fatalf("order: %s, %s", list[0].ID, list[1].ID)
	}
	latest, err := st.Latest("/repo/b")
	if err != nil || latest.ID != b.ID {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
	if _, err := st.Latest("/repo/none"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if _, err := st.Load("missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// A reopened store sees the same turns and builds the same history.
func TestReopenedStoreBuildsSameHistory(t *testing.T) {
	st := newStore(t)
	s, err := st.Create("/repo", "m")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.Recorder(s)
	if err != nil {
		t.Fatal(err)
	}
	addTurn(t, st, s, rec, "first")
	addTurn(t, st, s, rec, "second")
	want, err := st.History(t.Context(), rec, s)
	if err != nil || len(want.Messages) != 4 || len(want.Notices) != 0 {
		t.Fatalf("history=%+v err=%v", want, err)
	}

	again := session.NewStore(st.Root())
	again.DisableSync()
	s2, err := again.Latest("/repo")
	if err != nil || len(s2.Turns) != 2 {
		t.Fatalf("session=%+v err=%v", s2, err)
	}
	rec2, err := again.Recorder(s2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.History(t.Context(), rec2, s2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want.Messages))
	}
	for i := range got.Messages {
		if got.Messages[i].Text() != want.Messages[i].Text() {
			t.Fatalf("message %d: %q != %q", i,
				got.Messages[i].Text(), want.Messages[i].Text())
		}
	}
	before, err := again.HistoryBefore(t.Context(), rec2, s2, 1)
	if err != nil || len(before.Messages) != 2 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
}

// An unreadable turn is skipped with a notice, and the rest still count.
func TestHistorySkipsUnreadableTurn(t *testing.T) {
	st := newStore(t)
	s, err := st.Create("/repo", "m")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.Recorder(s)
	if err != nil {
		t.Fatal(err)
	}
	addTurn(t, st, s, rec, "first")
	addTurn(t, st, s, rec, "second")
	if err := os.WriteFile(rec.Path(s.Turns[0]), []byte("not a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := st.History(t.Context(), rec, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Messages) != 2 || h.Messages[0].Text() != "second" {
		t.Fatalf("messages=%v", h.Messages)
	}
	if len(h.Notices) != 1 || !strings.Contains(h.Notices[0], "Turn 1 skipped") {
		t.Fatalf("notices=%v", h.Notices)
	}
}

func TestLockExcludesSecondHolder(t *testing.T) {
	st := newStore(t)
	s, err := st.Create("/repo", "m")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock(s); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("second lock err=%v", err)
	}
	unlock()
	unlock2, err := st.Lock(s)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	unlock2()
}

// A session with no turn is neither saved nor listed, so a launch that ends
// at once cannot hide the session with real work.
func TestEmptySessionsAreNotListed(t *testing.T) {
	st := newStore(t)
	work, err := st.Create("/repo", "m")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.Recorder(work)
	if err != nil {
		t.Fatal(err)
	}
	addTurn(t, st, work, rec, "real work")
	empty, err := st.Create("/repo", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(empty); err != nil { // even an old empty file
		t.Fatal(err)
	}
	latest, err := st.Latest("/repo")
	if err != nil || latest.ID != work.ID {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
	list, _ := st.List("/repo")
	if len(list) != 1 {
		t.Fatalf("list=%v", list)
	}
}

// Discard removes an unsaved session directory and keeps a saved one.
func TestDiscard(t *testing.T) {
	st := newStore(t)
	s, _ := st.Create("/repo", "m")
	unlock, err := st.Lock(s)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	st.Discard(s)
	if _, err := os.Stat(filepath.Join(st.Root(), s.ID)); !os.IsNotExist(err) {
		t.Fatalf("dir kept: %v", err)
	}
	rec, _ := st.Recorder(s)
	addTurn(t, st, s, rec, "x")
	st.Discard(s)
	if _, err := st.Load(s.ID); err != nil {
		t.Fatalf("saved session lost: %v", err)
	}
}
