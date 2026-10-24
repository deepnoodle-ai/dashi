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

// heldStart holds the commit of turn turnID's turn_started step until
// release closes, then commits it, or with reject, refuses it with nothing
// written.
type heldStart struct {
	*dashi.MemoryRecorder
	turnID  string
	reject  bool
	entered chan struct{}
	release chan struct{}
}

func newHeldStart(turnID string, reject bool) *heldStart {
	return &heldStart{
		MemoryRecorder: dashi.NewMemoryRecorder(),
		turnID:         turnID,
		reject:         reject,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func (r *heldStart) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if req.Step.Kind != dashi.StepTurnStarted || req.Step.TurnID != r.turnID {
		return r.MemoryRecorder.Commit(ctx, req)
	}
	close(r.entered)
	<-r.release
	if r.reject {
		return &dashi.CommitError{NothingWritten: true, Err: dashitest.ErrInjected}
	}
	return r.MemoryRecorder.Commit(ctx, req)
}

// A steer sent to a turn that never began moves to the queue, so the next
// Start or RunQueue takes it.
func TestSessionLeftoversAfterTurnNeverStarted(t *testing.T) {
	rec := newHeldStart("s-1", true)
	s := handle(rec, dashitest.NewClient(dashitest.Reply("done")))
	s.New = true
	done := sendAsync(t.Context(), s, "start")
	<-rec.entered
	if q, err := s.Steer(t.Context(), "missed"); err != nil || q.TurnID != "s-1" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	close(rec.release)
	if r := <-done; r.err == nil {
		t.Fatal("the Start succeeded")
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d, want the missed steer", n)
	}
	s.Runner.Recorder = rec.MemoryRecorder
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if got := input(t, rec.MemoryRecorder, out.Turn.ID); !slices.Equal(got, []string{"missed"}) {
		t.Fatalf("input=%q", got)
	}
}

// A turn whose last commit lost its acknowledgement has ended, though the
// handle could not tell. The next Start puts the steer the turn missed in
// its input.
func TestSessionLeftoversAfterLostStopAck(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("done"), dashitest.Reply("fresh"))
	s := handle(&dashitest.FaultRecorder{
		Recorder: rec, Kind: dashi.StepTurnStopped, Fault: dashitest.LoseAck,
	}, client)
	s.New = true
	s.Agent.Limits.ModelCalls = 1
	done := sendAsync(t.Context(), s, "start")
	entered(t, client)
	if _, err := s.Steer(t.Context(), "missed"); err != nil {
		t.Fatal(err)
	}
	close(client.gate.release)
	<-done
	if v := load(t, rec, "s-1"); v.Next() != dashi.NextNone {
		t.Fatalf("next=%s, want an ended turn", v.Next())
	}
	out, err := s.Send(t.Context(), "new")
	if err != nil || out.Turn.ID != "s-2" {
		t.Fatalf("turn=%s err=%v", out.Turn.ID, err)
	}
	if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"missed", "new"}) {
		t.Fatalf("input=%q", got)
	}
}
