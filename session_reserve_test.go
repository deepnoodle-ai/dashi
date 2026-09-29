package dashi_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
)

// flakyLoad fails the next loads once its fault has fired.
type flakyLoad struct {
	*dashitest.FaultRecorder
	fails atomic.Int32
}

func (r *flakyLoad) Load(ctx context.Context, id string) (dashi.Turn, error) {
	if _, fired := r.Fired(); fired && r.fails.Add(-1) >= 0 {
		return dashi.Turn{}, errors.New("store unavailable")
	}
	return r.FaultRecorder.Load(ctx, id)
}

// Queued items a Start carries are consumed only when its turn_started
// step committed. Proof that it did not (a rejected command, a rejected or
// conflicting start commit) restores them in order; an unknown result loads
// the turn to decide; a failed load keeps them reserved until the next
// command settles them. An item is never delivered twice.
func TestSessionReservedItems(t *testing.T) {
	cases := []struct {
		name      string
		fault     dashitest.Fault
		kind      dashi.StepKind
		badAgent  bool
		loadFails int32
		consumed  bool
	}{
		{name: "start fails validation", badAgent: true},
		{name: "recorder rejects the start", fault: dashitest.Reject, kind: dashi.StepStarted},
		{name: "start write lost", fault: dashitest.LoseWrite, kind: dashi.StepStarted},
		{
			name:     "start acknowledgement lost",
			fault:    dashitest.LoseAck,
			kind:     dashi.StepStarted,
			consumed: true,
		},
		{
			name:     "model step rejected after the start",
			fault:    dashitest.Reject,
			kind:     dashi.StepModelIntent,
			consumed: true,
		},
		{
			name:      "load fails after an unknown result",
			fault:     dashitest.LoseWrite,
			kind:      dashi.StepStarted,
			loadFails: 2,
		},
	}
	for _, mode := range []string{"queue turn", "caller start"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				mem := dashi.NewMemoryRecorder()
				rec := &flakyLoad{FaultRecorder: &dashitest.FaultRecorder{
					Recorder: mem,
					Kind:     tc.kind,
					Fault:    tc.fault,
				}}
				rec.fails.Store(tc.loadFails)
				if tc.fault == "" {
					rec.FaultRecorder.Kind = "never"
				}
				client := dashitest.NewClient(
					dashitest.Reply("ok"),
					dashitest.Reply("ok"),
				)
				s := handle(rec, client)
				s.New = true
				for _, text := range []string{"a", "b"} {
					if _, err := s.Enqueue(t.Context(), text); err != nil {
						t.Fatal(err)
					}
				}
				if tc.badAgent {
					s.Agent.Provider = "other"
				}
				run := func(text string) error {
					if mode == "queue turn" {
						_, _, err := s.RunQueue(t.Context())
						return err
					}
					_, err := s.Send(t.Context(), text)
					return err
				}
				if err := run("x"); err == nil {
					t.Fatal("the failing run succeeded")
				}
				s.Agent.Provider = ""
				if tc.loadFails > 0 {
					// Still reserved after Open fails to settle them too;
					// Open itself works (TestSessionQueueBlocked).
					if st, err := s.Open(t.Context()); err != nil || st.Queued != 2 {
						t.Fatalf("open: %+v, %v", st, err)
					}
				}
				want := 2
				if tc.consumed {
					want = 0
				}
				if n := queued(t, s); n != want {
					t.Fatalf("queued=%d, want %d", n, want)
				}
				if !tc.consumed {
					// Restored in order: the next run carries them.
					if err := run("y"); err != nil {
						t.Fatal(err)
					}
					got := input(t, mem, "s-1")
					wantIn := []string{"a", "b"}
					if mode == "caller start" {
						wantIn = append(wantIn, "y")
					}
					if !slices.Equal(got, wantIn) {
						t.Fatalf("input=%q, want %q", got, wantIn)
					}
					requireTurns(t, mem, 1)
					return
				}
				// Consumed: turn 1 holds them. A later item and message
				// start turn 2 without them.
				wantIn := []string{"a", "b"}
				if mode == "caller start" {
					wantIn = append(wantIn, "x")
				}
				if got := input(t, mem, "s-1"); !slices.Equal(got, wantIn) {
					t.Fatalf("turn 1 input=%q, want %q", got, wantIn)
				}
				if _, err := s.Enqueue(t.Context(), "c"); err != nil {
					t.Fatal(err)
				}
				s.Exclusive = true // turn 1 has no worker
				if _, err := s.Send(t.Context(), "next"); err != nil {
					t.Fatal(err)
				}
				got := input(t, mem, "s-2")
				if !slices.Equal(got, []string{"c", "next"}) {
					t.Fatalf("turn 2 input=%q", got)
				}
				requireTurns(t, mem, 2)
			})
		}
	}
}

// writeOnly is a recorder that can only commit.
type writeOnly struct{ rec dashi.Recorder }

func (w writeOnly) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return w.rec.Commit(ctx, req)
}

// A Stream consumer that breaks after the turn_started event has seen the
// start commit, so the queued items it carried are consumed even when the
// recorder cannot load the turn to check.
func TestSessionStreamBreakWriteOnly(t *testing.T) {
	mem := dashi.NewMemoryRecorder()
	s := handle(writeOnly{mem}, dashitest.NewClient(dashitest.Reply("unused")))
	s.New = true
	if _, err := s.Enqueue(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	for ev, err := range s.Stream(t.Context(), dashi.Say("hi")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepStarted {
			break
		}
	}
	if st, err := s.Open(t.Context()); err != nil || st.Queued != 0 {
		t.Fatalf("open: %+v, %v", st, err)
	}
	if _, ran, err := s.RunQueue(t.Context()); err != nil || ran {
		t.Fatalf("run queue: %v, %v", ran, err)
	}
	if got := input(t, mem, "s-1"); !slices.Equal(got, []string{"a", "hi"}) {
		t.Fatalf("turn 1 input=%q", got)
	}
}

// ackLost loses the acknowledgement of turn s-1's start, whose record then
// cannot be read.
type ackLost struct {
	*dashi.MemoryRecorder
	lost atomic.Bool
}

func (r *ackLost) Commit(ctx context.Context, req dashi.CommitRequest) error {
	err := r.MemoryRecorder.Commit(ctx, req)
	if err == nil && req.Step.TurnID == "s-1" && req.Step.Kind == dashi.StepStarted {
		r.lost.Store(true)
		return errors.New("acknowledgement lost")
	}
	return err
}

func (r *ackLost) Load(ctx context.Context, id string) (dashi.Turn, error) {
	if id == "s-1" && r.lost.Load() {
		return dashi.Turn{}, errors.New("disk error")
	}
	return r.MemoryRecorder.Load(ctx, id)
}

// A reservation the session cannot settle holds back only the queue: new
// commands and Open run, each outcome warns queue_blocked, and RunQueue
// returns the error.
func TestSessionQueueBlocked(t *testing.T) {
	mem := dashi.NewMemoryRecorder()
	rec := &ackLost{MemoryRecorder: mem}
	s := handle(rec, dashitest.NewClient(dashitest.Reply("ok")))
	s.New = true
	s.Exclusive = true // so the session skips the unreadable turn 1
	if _, err := s.Enqueue(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(t.Context(), "hi"); err == nil {
		t.Fatal("the lost start succeeded")
	}
	out, err := s.Send(t.Context(), "next")
	if err != nil || out.Turn.ID != "s-2" ||
		!hasWarning(out.SessionWarnings, dashi.WarningQueueBlocked) {
		t.Fatalf("send: %v, turn %s, warnings %v", err, out.Turn.ID, out.SessionWarnings)
	}
	if got := input(t, mem, "s-2"); !slices.Equal(got, []string{"next"}) {
		t.Fatalf("turn 2 input=%q", got)
	}
	if st, err := s.Open(t.Context()); err != nil || st.Queued != 1 {
		t.Fatalf("open: %+v, %v", st, err)
	}
	_, ran, err := s.RunQueue(t.Context())
	if ran || err == nil || !strings.Contains(err.Error(), "stay reserved") {
		t.Fatalf("run queue: %v, %v", ran, err)
	}
	requireTurns(t, mem, 2)
}
