package session_test

import (
	"cmp"
	"context"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
)

// A steer that missed a paused turn is older than input queued while the
// turn waited, so it goes ahead of that input when a new Start closes the
// turn.
func TestSessionLeftoversKeepOrder(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("fresh"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "start")
	entered(t, client)
	if _, err := s.Steer(t.Context(), "older"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if r := <-done; r.err != nil || r.out.Next() != dashi.NextContinue {
		t.Fatalf("next=%s err=%v", r.out.Next(), r.err)
	}
	close(client.gate.release)
	if q, err := s.Steer(t.Context(), "newer"); err != nil || q.TurnID != "" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	out, err := s.Send(t.Context(), "new")
	if err != nil || out.Turn.ID != "s-2" {
		t.Fatalf("turn=%s err=%v", out.Turn.ID, err)
	}
	want := []string{"older", "newer", "new"}
	if got := input(t, rec, "s-2"); !slices.Equal(got, want) {
		t.Fatalf("input=%q, want %q", got, want)
	}
}

// A steer the turn missed keeps its arrival order relative to input queued
// while the turn ran: input queued before the steer stays ahead of it.
func TestSessionLeftoversAfterEarlierEnqueue(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("fresh"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "start")
	entered(t, client)
	if _, err := s.Enqueue(t.Context(), "E"); err != nil {
		t.Fatal(err)
	}
	if q, err := s.Steer(t.Context(), "S"); err != nil || q.TurnID != "s-1" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	cancel()
	if r := <-done; r.err != nil || r.out.Next() != dashi.NextContinue {
		t.Fatalf("next=%s err=%v", r.out.Next(), r.err)
	}
	close(client.gate.release)
	out, err := s.Send(t.Context(), "new")
	if err != nil || out.Turn.ID != "s-2" {
		t.Fatalf("turn=%s err=%v", out.Turn.ID, err)
	}
	want := []string{"E", "S", "new"}
	if got := input(t, rec, "s-2"); !slices.Equal(got, want) {
		t.Fatalf("input=%q, want %q", got, want)
	}
}

// A turn that ends when its context does, under ContextCancel, moves a
// steer it never saw to the queue, from Run and from Stream.
func TestSessionLeftoversAfterContextCancel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		rec := dashi.NewMemoryRecorder()
		client := held(1)
		s := handle(rec, client)
		s.New = true
		s.Runner.OnContextDone = dashi.ContextCancel
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			entered(t, client)
			if _, err := s.Steer(t.Context(), "missed"); err != nil {
				t.Error(err)
			}
			cancel()
		}()
		var err error
		if stream {
			for _, e := range s.Stream(ctx, dashi.Say("start")) {
				err = cmp.Or(e, err)
			}
		} else {
			_, err = s.Send(ctx, "start")
		}
		if err != nil {
			t.Fatalf("stream=%v err=%v", stream, err)
		}
		if v := load(t, rec, "s-1"); v.Next() != dashi.NextNone {
			t.Fatalf("stream=%v next=%s, want an ended turn", stream, v.Next())
		}
		if n := queued(t, s); n != 1 {
			t.Fatalf("stream=%v queued=%d, want the missed steer", stream, n)
		}
	}
}

// A Stream consumer that stops at the step that ends its turn leaves no
// steer stranded in the inbox: the steer the turn missed moves to the
// queue.
func TestSessionLeftoversAfterConsumerStops(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("done"))
	s := handle(rec, client)
	s.New = true
	s.Agent.Limits.ModelCalls = 1
	go func() {
		entered(t, client)
		if _, err := s.Steer(t.Context(), "missed"); err != nil {
			t.Error(err)
		}
		close(client.gate.release)
	}()
	for ev := range s.Stream(t.Context(), dashi.Say("start")) {
		if ev.Step != nil && ev.Step.Kind == dashi.StepTurnStopped {
			break
		}
	}
	if v := load(t, rec, "s-1"); v.Next() != dashi.NextNone {
		t.Fatalf("next=%s, want an ended turn", v.Next())
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d, want the missed steer", n)
	}
}
