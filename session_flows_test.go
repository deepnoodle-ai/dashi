package dashi_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// TestSessionIdempotency retries a keyed Start while its turn runs, after
// it ends, after queue and compaction turns, and with other input.
func TestSessionIdempotency(t *testing.T) {
	keyed := func(key, text string) dashi.Start {
		start := dashi.Say(text)
		start.CommandID = key
		return start
	}
	t.Run("while running", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		g := newGate()
		a := handle(rec, gatedClient{dashitest.NewClient(dashitest.Reply("ok")), g})
		a.New = true
		done := make(chan error, 1)
		go func() {
			_, err := a.Run(context.Background(), keyed("k", "hello"))
			done <- err
		}()
		g.requireEntered(t, done)
		// Another worker's handle cannot know the turn is live.
		b := handle(rec, dashitest.NewClient())
		_, err := b.Run(t.Context(), keyed("k", "hello"))
		busy := requireBusy(t, err, dashi.NextResume, false)
		if !busy.Duplicate || busy.TurnID != dashi.TurnID("s", 1) {
			t.Fatalf("busy = %+v; want a duplicate of turn 1", busy)
		}
		// The handle running it does.
		_, err = a.Run(t.Context(), keyed("k", "hello"))
		if busy := requireBusy(t, err, dashi.NextRunning, false); !busy.Duplicate {
			t.Fatalf("busy = %+v; want a duplicate", busy)
		}
		close(g.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		requireTurns(t, rec, 1)
	})
	t.Run("after it ended", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(dashitest.Reply("first"))
		a := handle(rec, client)
		a.New = true
		first, err := a.Run(t.Context(), keyed("k", "hello"))
		if err != nil {
			t.Fatal(err)
		}
		b := handle(rec, client)
		b.New = true // a retry of the first Start is not ErrSessionExists
		out, err := b.Run(t.Context(), keyed("k", "hello"))
		if err != nil || out.Turn.ID != first.Turn.ID || out.Answer != "first" {
			t.Fatalf("retry: %v, turn %s %q", err, out.Turn.ID, out.Answer)
		}
		if n := len(client.Inputs()); n != 1 {
			t.Fatalf("model calls = %d; want 1", n)
		}
		requireTurns(t, rec, 1)
	})
	t.Run("after queue and compaction turns", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(
			dashitest.Reply("first"),
			dashitest.Reply("queued"),
			dashitest.Reply("summary"),
			dashitest.Reply("other"),
			dashitest.Reply("again"),
		)
		s := handle(rec, client)
		s.New = true
		if _, err := s.Run(t.Context(), keyed("k", "hello")); err != nil {
			t.Fatal(err)
		}
		// No queue or compactor exists yet, so the turns are made directly,
		// with the keys and origins those would record.
		queue := keyed(`queue:["n1"]`, "a notice")
		queue.Origin = dashi.StartScheduled
		compact := keyed("compact:1", "summarize")
		compact.Origin = dashi.StartOperator
		for _, c := range []dashi.Start{queue, compact} {
			if _, err := s.Run(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		}
		out, err := handle(rec, client).Run(t.Context(), keyed("k", "hello"))
		if err != nil || out.Turn.ID != dashi.TurnID("s", 1) {
			t.Fatalf("retry: %v, turn %s", err, out.Turn.ID)
		}
		requireTurns(t, rec, 3)
		// A newer caller turn ends the window: the old key starts anew.
		if _, err := s.Run(t.Context(), keyed("k2", "other")); err != nil {
			t.Fatal(err)
		}
		out, err = s.Run(t.Context(), keyed("k", "hello"))
		if err != nil || out.Turn.ID != dashi.TurnID("s", 5) {
			t.Fatalf("outside the window: %v, turn %s", err, out.Turn.ID)
		}
	})
	t.Run("different input", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		s := handle(rec, dashitest.NewClient(dashitest.Reply("first")))
		s.New = true
		if _, err := s.Run(t.Context(), keyed("k", "hello")); err != nil {
			t.Fatal(err)
		}
		_, err := s.Run(t.Context(), keyed("k", "goodbye"))
		if !errors.Is(err, dashi.ErrCommandConflict) {
			t.Fatalf("err = %v; want ErrCommandConflict", err)
		}
		requireTurns(t, rec, 1)
	})
	t.Run("racing workers", func(t *testing.T) {
		// Worker b chooses turn 1 before worker a commits it, with the same
		// key and a different agent, so the engine's digests differ. b
		// loses at commit, reloads, and finds a's turn as its retry.
		rec := dashi.NewMemoryRecorder()
		a := handle(rec, dashitest.NewClient(dashitest.Reply("from a")))
		a.New = true
		bClient := dashitest.NewClient(dashitest.Reply("from b"))
		race := &raceRecorder{MemoryRecorder: rec}
		race.before = func() {
			if _, err := a.Run(context.Background(), keyed("k", "hello")); err != nil {
				t.Error(err)
			}
		}
		b := handle(race, bClient)
		b.New = true
		b.Agent.Instructions = "Be brief."
		out, err := b.Run(t.Context(), keyed("k", "hello"))
		if err != nil || out.Turn.ID != dashi.TurnID("s", 1) || out.Answer != "from a" {
			t.Fatalf("b: %v, turn %s %q", err, out.Turn.ID, out.Answer)
		}
		if n := len(bClient.Inputs()); n != 0 {
			t.Fatalf("b made %d model calls", n)
		}
		requireTurns(t, rec, 1)
	})
}

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

// TestSessionCarryForward merges each new turn's agent field by field from
// Start.Agent, Session.Agent, and the last user turn.
func TestSessionCarryForward(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		dashitest.Reply("two"),
		dashitest.Reply("three"),
		dashitest.Reply("four"),
	)
	s := handle(rec, client)
	s.New = true
	s.Agent = dashi.Agent{
		Model:        dashitest.Model,
		Instructions: "Be brief.",
		Tools:        []string{"lookup"},
		Limits: dashi.Limits{
			ModelAttempts: 3,
			Deadline:      time.Now().Add(time.Hour),
		},
	}
	first, err := s.Send(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	// A later handle sets one field; the rest carries from turn 1.
	next := handle(rec, client)
	next.Agent = dashi.Agent{Settings: llm.Settings{MaxTokens: 100}}
	out, err := next.Send(t.Context(), "two")
	if err != nil {
		t.Fatal(err)
	}
	got := out.Agent
	switch {
	case got.Instructions != "Be brief.",
		got.Model != dashitest.Model,
		got.Settings.MaxTokens != 100,
		got.Limits.ModelAttempts != 3,
		len(got.Tools) != 1:
		t.Fatalf("carried agent = %+v", got)
	case !got.Limits.Deadline.IsZero():
		t.Fatal("the deadline carried forward")
	case got.Revision == first.Agent.Revision:
		t.Fatal("the derived revision carried forward")
	}
	// A scheduled turn's agent never carries.
	scheduled := dashi.Say("three")
	scheduled.Origin = dashi.StartScheduled
	scheduled.Agent = dashi.Agent{Instructions: "Summarize.", NoTools: true}
	if _, err := next.Run(t.Context(), scheduled); err != nil {
		t.Fatal(err)
	}
	out, err = handle(rec, client).Send(t.Context(), "four")
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent.Instructions != "Be brief." || out.Agent.NoTools ||
		out.Agent.Settings.MaxTokens != 100 {
		t.Fatalf("agent after a scheduled turn = %+v", out.Agent)
	}
}

// TestSessionAnswer records each answer, then continues, in one call.
func TestSessionAnswer(t *testing.T) {
	client := dashitest.NewClient(
		dashitest.CallTool("call-1", "charge", `{}`),
		dashitest.Reply("I did not charge the card."),
	)
	s := handle(nil, client)
	s.New = true
	s.Runner.Tools = (&chargeTool{}).tools()
	s.Runner.Hooks = dashi.RequireApproval("charge")
	out, err := s.Send(t.Context(), "pay")
	if err != nil || out.Next() != dashi.NextAnswer {
		t.Fatalf("send: %v, next %s", err, out.Next())
	}
	out, err = s.Answer(t.Context(), []dashi.Settle{out.Pending[0].Deny("the user said no")})
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("answer: %v, stop %+v", err, out.Stop)
	}
	if got := texts(client.Inputs()[1].Messages); !strings.Contains(got, "the user said no") {
		t.Fatalf("the model did not see the denial: %q", got)
	}
}

// TestSessionAnswerOptions passes Answer's options to the runs it makes: a
// leasing worker's WithOwner reaches the recorder on the Settle and on the
// Continue.
func TestSessionAnswerOptions(t *testing.T) {
	client := dashitest.NewClient(
		dashitest.CallTool("call-1", "charge", `{}`),
		dashitest.Reply("I did not charge the card."),
	)
	rec := &ownerRecorder{MemoryRecorder: dashi.NewMemoryRecorder()}
	s := handle(rec, client)
	s.New = true
	s.Runner.Tools = (&chargeTool{}).tools()
	s.Runner.Hooks = dashi.RequireApproval("charge")
	out, err := s.Send(t.Context(), "pay")
	if err != nil || out.Next() != dashi.NextAnswer {
		t.Fatalf("send: %v, next %s", err, out.Next())
	}
	// Worker w1 leases the turn; a commit without its owner is rejected.
	if err := rec.Claim(t.Context(), out.Turn.ID, "w1"); err != nil {
		t.Fatal(err)
	}
	rec.owners = nil
	deny := []dashi.Settle{out.Pending[0].Deny("no")}
	out, err = s.Answer(t.Context(), deny, dashi.WithOwner("w1"))
	if err != nil || out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("answer: %v, stop %+v", err, out.Stop)
	}
	if len(rec.owners) < 2 {
		t.Fatalf("commits = %d; want the settle and the continue", len(rec.owners))
	}
	for i, owner := range rec.owners {
		if owner != "w1" {
			t.Fatalf("commit %d owner = %q; want w1", i, owner)
		}
	}
}

// ownerRecorder notes the owner of each commit.
type ownerRecorder struct {
	*dashi.MemoryRecorder
	mu     sync.Mutex
	owners []string
}

func (r *ownerRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	r.mu.Lock()
	r.owners = append(r.owners, req.Owner)
	r.mu.Unlock()
	return r.MemoryRecorder.Commit(ctx, req)
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

// raceRecorder runs before once, ahead of the first turn_started commit,
// so another worker commits the same turn first.
type raceRecorder struct {
	*dashi.MemoryRecorder
	before func()
	done   bool
}

func (r *raceRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if req.Step.Kind == dashi.StepStarted && !r.done {
		r.done = true
		r.before()
	}
	return r.MemoryRecorder.Commit(ctx, req)
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

// TestSessionProbingReads finds the last turn of long sessions by probing
// in at most 2·log2(n)+2 reads, plus one to load the last turn, through
// StepReader and through Loader alone. The bound belongs to the session's
// search, so it is checked here on MemoryRecorder rather than on every store
// in dashitest.
func TestSessionProbingReads(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	for _, n := range []int{64, 1000} {
		id := fmt.Sprintf("probe-%d", n)
		for i := 1; i <= n; i++ {
			turn := dashi.TurnID(id, i)
			err := rec.Commit(t.Context(), dashi.CommitRequest{Step: dashi.Step{
				Version:       1,
				TurnID:        turn,
				Seq:           1,
				At:            time.Now(),
				Kind:          dashi.StepStarted,
				CommandID:     "start",
				CommandDigest: "digest",
				Started: &dashi.StartFact{
					Agent: dashi.Agent{
						Revision: "r1",
						Provider: "test",
						API:      "test",
						Model:    "test",
					},
					Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
				},
			}})
			if err != nil {
				t.Fatalf("seed %s: %v", turn, err)
			}
		}
		limit := int64(2*math.Log2(float64(n))+2) + 1
		loads := &countingLoader{rec: rec}
		steps := &countingReader{countingLoader{rec: rec}}
		for _, c := range []struct {
			name  string
			rec   dashi.Recorder
			reads *int64
		}{
			{"Loader", loads, &loads.reads},
			{"StepReader", steps, &steps.reads},
		} {
			s := &dashi.Session{ID: id, Runner: dashi.Runner{Recorder: c.rec}}
			st, err := s.Open(t.Context())
			if err != nil || st.Turns != n {
				t.Fatalf("%s, %d turns: open %v, turns %d", c.name, n, err, st.Turns)
			}
			if *c.reads > limit {
				t.Fatalf("%s, %d turns: %d reads; want at most %d", c.name, n, *c.reads, limit)
			}
		}
	}
}

// countingLoader offers only Loader, and counts reads.
type countingLoader struct {
	rec   *dashi.MemoryRecorder
	reads int64
}

func (c *countingLoader) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return c.rec.Commit(ctx, req)
}

func (c *countingLoader) Load(ctx context.Context, id string) (dashi.Turn, error) {
	c.reads++
	return c.rec.Load(ctx, id)
}

// countingReader adds StepReader, whose reads it also counts.
type countingReader struct {
	countingLoader
}

func (c *countingReader) StepsAfter(
	ctx context.Context,
	id string,
	after int64,
	limit int,
) ([]dashi.Step, error) {
	c.reads++
	return c.rec.StepsAfter(ctx, id, after, limit)
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
