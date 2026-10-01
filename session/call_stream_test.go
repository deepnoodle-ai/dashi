package session_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// streamed is what a Session.Stream yielded, by turn.
type streamed struct {
	// order lists turn IDs in the order their first event came.
	order []string
	// steps holds the kinds of step events per turn.
	steps map[string][]dashi.StepKind
	// outcomes lists the turns of the RunResult events, in order.
	outcomes []string
	byTurn   map[string]dashi.RunResult
	last     dashi.RunResult
	errs     []error
}

func drain(t *testing.T, seq func(func(dashi.Event, error) bool)) streamed {
	t.Helper()
	got := streamed{
		steps:  map[string][]dashi.StepKind{},
		byTurn: map[string]dashi.RunResult{},
	}
	for ev, err := range seq {
		if err != nil {
			got.errs = append(got.errs, err)
		}
		if ev.Result == nil && ev.Step == nil && ev.Delta == nil &&
			ev.Progress == nil {
			continue
		}
		if ev.TurnID == "" {
			t.Fatalf("event without a turn ID: %+v", ev)
		}
		if !slices.Contains(got.order, ev.TurnID) {
			got.order = append(got.order, ev.TurnID)
		}
		switch {
		case ev.Result != nil:
			if ev.Result.Turn.ID != ev.TurnID {
				t.Fatalf("outcome of %s tagged %s", ev.Result.Turn.ID, ev.TurnID)
			}
			got.outcomes = append(got.outcomes, ev.TurnID)
			got.byTurn[ev.TurnID] = *ev.Result
			got.last = *ev.Result
		case ev.Step != nil:
			if ev.Step.TurnID != ev.TurnID {
				t.Fatalf("step of %s tagged %s", ev.Step.TurnID, ev.TurnID)
			}
			got.steps[ev.TurnID] = append(got.steps[ev.TurnID], ev.Step.Kind)
		}
	}
	return got
}

// A session Stream spans the closing step, the caller's turn, and the
// queue turn after it; each event carries its turn ID and each turn ends
// with its own RunResult event.
func TestSessionStreamAcrossTurns(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("two"), dashitest.Reply("three"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "first")
	entered(t, client)
	cancel()
	<-done
	close(client.gate.release)
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("note")),
	}); err != nil {
		t.Fatal(err)
	}

	got := drain(t, s.Stream(t.Context(), dashi.Say("second")))
	if len(got.errs) != 0 {
		t.Fatal(got.errs)
	}
	if !slices.Equal(got.order, []string{"s-1", "s-2", "s-3"}) ||
		!slices.Equal(got.outcomes, []string{"s-1", "s-2", "s-3"}) {
		t.Fatalf("order=%v outcomes=%v", got.order, got.outcomes)
	}
	// Turn 1 shows only its closing step.
	if !slices.Equal(got.steps["s-1"], []dashi.StepKind{dashi.StepTurnStopped}) {
		t.Fatalf("turn 1 steps=%v", got.steps["s-1"])
	}
	for _, id := range []string{"s-2", "s-3"} {
		steps := got.steps[id]
		if steps[0] != dashi.StepTurnStarted || steps[len(steps)-1] != dashi.StepTurnStopped {
			t.Fatalf("%s steps=%v", id, steps)
		}
	}
	if got.last.Turn.ID != "s-3" || got.last.Reply != "three" {
		t.Fatalf("last=%s %q", got.last.Turn.ID, got.last.Reply)
	}
	if w := got.byTurn["s-2"].SessionWarnings; !hasWarning(w, session.WarningTurnClosed) {
		t.Fatalf("turn 2 warnings=%+v", w)
	}
	if v := got.byTurn["s-1"]; v.Stop.Reason != session.CloseReason {
		t.Fatalf("turn 1 stop=%+v", v.Stop)
	}
}

// Breaking the range interrupts the turn, and no queue turn starts.
func TestSessionStreamBreak(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("unused"))
	s := handle(rec, client)
	s.New = true
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("note")),
	}); err != nil {
		t.Fatal(err)
	}
	for ev, err := range s.Stream(t.Context(), dashi.Say("hi")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepModelCallStarted {
			break
		}
	}
	requireTurns(t, rec, 1)
	if v := load(t, rec, "s-1"); v.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("stop=%+v", v.Stop)
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	// An error before any turn runs is yielded alone.
	other := handle(rec, client)
	other.New = true
	got := drain(t, other.Stream(t.Context(), dashi.Say("x")))
	if len(got.errs) != 1 || !errors.Is(got.errs[0], session.ErrExists) ||
		len(got.order) != 0 {
		t.Fatalf("errs=%v order=%v", got.errs, got.order)
	}
}

// A Stream of a retried Start whose turn is running follows that turn and
// ends with its outcome, without starting another turn.
func TestSessionStreamFollowsDuplicate(t *testing.T) {
	rec := newLiveRecorder()
	client := held(1, dashitest.Reply("answer"))
	s := handle(rec, client)
	s.New = true
	start := dashi.Say("hi")
	start.CommandID = "k"
	done := make(chan result, 1)
	go func() {
		out, err := s.Run(t.Context(), start)
		done <- result{out, err}
	}()
	entered(t, client)
	rec.set(session.LivenessLive)
	retry := handle(rec, client)
	// Release the model call only once the retry follows the running
	// turn: a retry planned after the turn ended would get its outcome
	// without step events.
	following := make(chan struct{})
	var once sync.Once
	seq := retry.Stream(t.Context(), start)
	signaled := func(yield func(dashi.Event, error) bool) {
		for ev, err := range seq {
			if ev.Step != nil {
				once.Do(func() { close(following) })
			}
			if !yield(ev, err) {
				return
			}
		}
	}
	events := make(chan streamed, 1)
	go func() { events <- drain(t, signaled) }()
	select {
	case <-following:
	case got := <-events:
		t.Fatalf("the retry did not follow: errs=%v outcomes=%v", got.errs, got.outcomes)
	}
	close(client.gate.release)
	awaitSend(t, done)
	got := <-events
	if len(got.errs) != 0 || !slices.Equal(got.outcomes, []string{"s-1"}) ||
		got.last.Reply != "answer" {
		t.Fatalf("errs=%v outcomes=%v answer=%q", got.errs, got.outcomes, got.last.Reply)
	}
	steps := got.steps["s-1"]
	if len(steps) == 0 || steps[0] != dashi.StepTurnStarted || steps[len(steps)-1] != dashi.StepTurnStopped {
		t.Fatalf("steps=%v", steps)
	}
	requireTurns(t, rec, 1)
}

// staleLoad serves each turn once without its last step, as a read that
// raced the turn's stop would.
type staleLoad struct {
	*liveRecorder
	stale atomic.Bool
}

func (r *staleLoad) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := r.liveRecorder.Load(ctx, id)
	if err == nil && len(turn.Steps) > 1 && r.stale.CompareAndSwap(true, false) {
		turn.Steps = turn.Steps[:len(turn.Steps)-1]
	}
	return turn, err
}

// A followed duplicate that stopped after the duplicate check, at a stop
// that has not ended it, is reported at once rather than waited on.
func TestSessionStreamFollowsStoppedDuplicate(t *testing.T) {
	rec := &staleLoad{liveRecorder: newLiveRecorder()}
	client := dashitest.NewClient()
	s := handle(rec, client)
	s.New = true
	pause := make(chan struct{})
	close(pause)
	start := dashi.Say("hi")
	start.CommandID = "k"
	if _, err := s.Run(t.Context(), start, dashi.WithPause(pause)); err != nil {
		t.Fatal(err)
	}
	// The duplicate check reads the turn before its stop, which a live
	// worker makes NextRunning; the follow then loads the stopped turn.
	rec.set(session.LivenessLive)
	rec.stale.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := drain(t, handle(rec, client).Stream(ctx, start))
	if ctx.Err() != nil {
		t.Fatal("the stream waited on a stopped turn")
	}
	if len(got.errs) != 0 || !slices.Equal(got.outcomes, []string{"s-1"}) ||
		got.last.Next() != dashi.NextContinue {
		t.Fatalf("errs=%v outcomes=%v next=%s", got.errs, got.outcomes, got.last.Next())
	}
	requireTurns(t, rec, 1)
}

// Turns yields every record, an unreadable one with its error; History is
// the conversation across turns.
func TestSessionTurnsAndHistory(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	for _, text := range []string{"a", "b"} {
		if _, err := s.Send(t.Context(), text); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.History(t.Context())
	if err != nil || texts(history) != "a|one|b|two" {
		t.Fatalf("history=%q err=%v", texts(history), err)
	}
	projected, err := s.ProjectedHistory(t.Context())
	if err != nil || texts(projected) != texts(history) {
		t.Fatalf("projected=%q err=%v", texts(projected), err)
	}
	broken := &brokenLoader{MemoryRecorder: rec, id: "s-1"}
	var ids []string
	var errs int
	for turn, err := range handle(broken, client).Turns(t.Context()) {
		ids = append(ids, turn.ID)
		if err != nil {
			errs++
		}
	}
	if !slices.Equal(ids, []string{"s-1", "s-2"}) || errs != 1 {
		t.Fatalf("ids=%v errs=%d", ids, errs)
	}
}

// Breaking the range at the closing step leaves the handle idle: the new
// turn never started, its queued user items stay, and the next Send runs.
func TestSessionStreamBreakWhileClosing(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("fresh"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "first")
	entered(t, client)
	cancel()
	<-done
	close(client.gate.release)
	if _, err := s.Enqueue(t.Context(), "typed"); err != nil {
		t.Fatal(err)
	}
	for ev := range s.Stream(t.Context(), dashi.Say("second")) {
		if ev.Step == nil || ev.Step.Kind != dashi.StepTurnStopped || ev.TurnID != "s-1" {
			t.Fatalf("first event=%+v", ev)
		}
		break
	}
	requireTurns(t, rec, 1)
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	out, err := s.Send(t.Context(), "third")
	if err != nil || out.Turn.ID != "s-2" {
		t.Fatalf("turn=%s err=%v", out.Turn.ID, err)
	}
	if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"typed", "third"}) {
		t.Fatalf("input=%q", got)
	}
}
