package dashi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// TestSessionOpenAndCreate covers New, ErrSessionNotFound, and
// ErrSessionExists, and that Open runs nothing.
func TestSessionOpenAndCreate(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"))
	missing := handle(rec, client)
	if _, err := missing.Open(t.Context()); !errors.Is(err, dashi.ErrSessionNotFound) {
		t.Fatalf("open: %v; want ErrSessionNotFound", err)
	}
	if _, err := missing.Send(t.Context(), "hi"); !errors.Is(err, dashi.ErrSessionNotFound) {
		t.Fatalf("send: %v; want ErrSessionNotFound", err)
	}
	if _, err := missing.Run(t.Context(), dashi.Continue{}); !errors.Is(err, dashi.ErrSessionNotFound) {
		t.Fatalf("continue: %v; want ErrSessionNotFound", err)
	}
	s := handle(rec, client)
	s.New = true
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 0 || st.Next != dashi.NextNone {
		t.Fatalf("open new: %v, %+v", err, st)
	}
	for range 2 {
		if _, err := s.Send(t.Context(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	again := handle(rec, client)
	again.New = true
	if _, err := again.Send(t.Context(), "hi"); !errors.Is(err, dashi.ErrSessionExists) {
		t.Fatalf("send: %v; want ErrSessionExists", err)
	}
	st, err := handle(rec, client).Open(t.Context())
	if err != nil || st.Turns != 2 || st.Next != dashi.NextNone || st.Last.Answer != "two" {
		t.Fatalf("open: %v, %+v", err, st)
	}
	if n := len(client.Inputs()); n != 2 {
		t.Fatalf("model calls = %d; want 2", n)
	}
}

// TestSessionEmptyID generates the ID at the first Start and warns once
// that the session lives in memory.
func TestSessionEmptyID(t *testing.T) {
	s := &dashi.Session{
		Runner: dashi.Runner{Client: dashitest.NewClient(
			dashitest.Reply("one"),
			dashitest.Reply("two"),
		)},
		Agent: dashi.Agent{Model: dashitest.Model},
	}
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 0 {
		t.Fatalf("open: %v, %+v", err, st)
	}
	out, err := s.Send(t.Context(), "hi")
	if err != nil || s.ID == "" || out.Turn.ID != dashi.TurnID(s.ID, 1) {
		t.Fatalf("send: %v, id %q, turn %s", err, s.ID, out.Turn.ID)
	}
	if !hasWarning(out.SessionWarnings, dashi.WarningMemoryOnly) {
		t.Fatalf("warnings = %v; want memory_only", out.SessionWarnings)
	}
	out, err = s.Send(t.Context(), "again")
	if err != nil || len(out.SessionWarnings) != 0 {
		t.Fatalf("second send: %v, warnings %v", err, out.SessionWarnings)
	}
}

// TestSessionHistory sends each new turn the projected history of the
// turns before it, and a Continue the history before its own turn.
func TestSessionHistory(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	pause := make(chan struct{})
	close(pause)
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	if _, err := s.Send(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(t.Context(), "second", dashi.WithPause(pause)); err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(t.Context(), dashi.Continue{})
	if err != nil || out.Answer != "two" {
		t.Fatalf("continue: %v, %q", err, out.Answer)
	}
	if hasWarning(out.Warnings, dashi.WarningHistoryMismatch) {
		t.Fatal("the Continue history differs from the Start history")
	}
	got := texts(client.Inputs()[1].Messages)
	want := "first|one|second"
	if got != want {
		t.Fatalf("second call saw %q; want %q", got, want)
	}
}

// TestSessionUnreadableTurn leaves a turn that cannot be loaded out of the
// history with a warning; an unreadable last turn is an error.
func TestSessionUnreadableTurn(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		dashitest.Reply("two"),
		dashitest.Reply("three"),
	)
	s := handle(rec, client)
	s.New = true
	for range 2 {
		if _, err := s.Send(t.Context(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	broken := &brokenLoader{MemoryRecorder: rec, id: dashi.TurnID("s", 1)}
	out, err := handle(broken, client).Send(t.Context(), "third")
	if err != nil || !hasWarning(out.SessionWarnings, dashi.WarningTurnUnreadable) {
		t.Fatalf("send: %v, warnings %v", err, out.SessionWarnings)
	}
	if got := texts(client.Inputs()[2].Messages); got != "hi|two|third" {
		t.Fatalf("third call saw %q", got)
	}
	broken.id = dashi.TurnID("s", 3)
	if _, err := handle(broken, client).Send(t.Context(), "fourth"); err == nil {
		t.Fatal("an unreadable last turn was accepted")
	}
}

// texts joins the text of each message, in order.
func texts(messages []llm.Message) string {
	var parts []string
	for _, m := range messages {
		if text := m.Text(); text != "" {
			parts = append(parts, text)
		}
		for _, c := range m.Content {
			if c.ToolResult == nil {
				continue
			}
			for _, block := range c.ToolResult.Output.Content {
				if block.Text != nil {
					parts = append(parts, block.Text.Text)
				}
			}
		}
	}
	return strings.Join(parts, "|")
}

// brokenLoader fails to load one turn.
type brokenLoader struct {
	*dashi.MemoryRecorder
	id string
}

func (r *brokenLoader) Load(ctx context.Context, id string) (dashi.Turn, error) {
	if id == r.id {
		return dashi.Turn{}, errors.New("disk error")
	}
	return r.MemoryRecorder.Load(ctx, id)
}

// TestSessionUsesIndex finds the last turn from a SessionIndex without
// probing.
func TestSessionUsesIndex(t *testing.T) {
	rec := &indexed{MemoryRecorder: dashi.NewMemoryRecorder()}
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	for range 2 {
		if _, err := s.Send(t.Context(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	rec.calls = 0
	st, err := handle(rec, client).Open(t.Context())
	if err != nil || st.Turns != 2 || rec.calls != 1 {
		t.Fatalf("open: %v, turns %d, index calls %d", err, st.Turns, rec.calls)
	}
}

// indexed answers LastTurn from a counter it keeps as turns start, and
// fails any probe.
type indexed struct {
	*dashi.MemoryRecorder
	last  int
	calls int
}

func (r *indexed) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == dashi.StepStarted {
		r.last++
	}
	return nil
}

func (r *indexed) LastTurn(context.Context, string) (int, error) {
	r.calls++
	return r.last, nil
}

func (r *indexed) StepsAfter(context.Context, string, int64, int) ([]dashi.Step, error) {
	return nil, errors.New("probed despite the index")
}
