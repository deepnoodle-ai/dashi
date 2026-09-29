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
					// Still reserved: the next command cannot settle them
					// either, and runs nothing.
					if _, err := s.Open(t.Context()); err == nil ||
						!strings.Contains(err.Error(), "stay reserved") {
						t.Fatalf("open: %v", err)
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
