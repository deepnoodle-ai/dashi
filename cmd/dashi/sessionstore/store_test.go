package sessionstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
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

func newStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	st := sessionstore.NewStore(t.TempDir())
	st.DisableSync()
	return st
}

// chat returns a session handle on the records of s.
func chat(st *sessionstore.Store, s *sessionstore.Session, rec dashi.Recorder) *session.Session {
	return &session.Session{
		ID:     s.ID,
		New:    !st.HasTurns(s),
		Runner: dashi.Runner{Client: echoClient{}, Recorder: rec},
		Agent:  dashi.Agent{Model: "fixture"},
	}
}

// addTurn runs one turn of s and saves its metadata.
func addTurn(
	t *testing.T,
	st *sessionstore.Store,
	s *sessionstore.Session,
	rec dashi.Recorder,
	text string,
) {
	t.Helper()
	out, err := chat(st, s, rec).Send(t.Context(), text)
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
}

func TestCreateListLatestByDirectory(t *testing.T) {
	st := newStore(t)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	tick := 0
	st = sessionstore.WithClock(st, func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	})
	create := func(dir string) *sessionstore.Session {
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
	if _, err := st.Latest("/repo/none"); !errors.Is(err, sessionstore.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if _, err := st.Load("missing"); !errors.Is(err, sessionstore.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// A reopened store finds the same turns, and a session handle on it builds
// the same history.
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
	want, err := chat(st, s, rec).History(t.Context())
	if err != nil || len(want) != 4 {
		t.Fatalf("history=%v err=%v", want, err)
	}

	again := sessionstore.NewStore(st.Root())
	again.DisableSync()
	s2, err := again.Latest("/repo")
	if err != nil || s2.ID != s.ID {
		t.Fatalf("session=%+v err=%v", s2, err)
	}
	rec2, err := again.Recorder(s2)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rec2.LastTurn(t.Context(), s2.ID); n != 2 || err != nil {
		t.Fatalf("last turn=%d err=%v", n, err)
	}
	got, err := chat(again, s2, rec2).History(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Text() != want[i].Text() {
			t.Fatalf("message %d: %q != %q", i, got[i].Text(), want[i].Text())
		}
	}
}

// A damaged turn log does not hide the turns after it: the recorder finds
// the last turn without reading the logs, and the history skips the damaged
// turn with a warning. An empty log, as a rejected first write leaves, is
// no turn.
func TestDamagedTurnIsSkipped(t *testing.T) {
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
	if err := os.WriteFile(rec.Path(session.TurnID(s.ID, 1)), []byte("not a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := rec.Path(session.TurnID(s.ID, 3))
	if err := os.MkdirAll(filepath.Dir(empty), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := rec.LastTurn(t.Context(), s.ID); n != 2 || err != nil {
		t.Fatalf("last turn=%d err=%v", n, err)
	}
	h, err := chat(st, s, rec).History(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].Text() != "second" {
		t.Fatalf("messages=%v", h)
	}
	out, err := chat(st, s, rec).Send(t.Context(), "third")
	if err != nil || !hasWarning(out.SessionWarnings, dashi.WarningTurnUnreadable) ||
		out.Turn.ID != session.TurnID(s.ID, 3) {
		t.Fatalf("turn=%s warnings=%v err=%v", out.Turn.ID, out.SessionWarnings, err)
	}
}

func hasWarning(ws []dashi.Warning, code dashi.WarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
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
	if _, err := st.Lock(s); !errors.Is(err, sessionstore.ErrBusy) {
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
