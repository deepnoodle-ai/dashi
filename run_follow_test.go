package dashi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// setFollowIntervals shortens Follow's intervals for one test.
func setFollowIntervals(t *testing.T, poll, backstop, resting time.Duration) {
	t.Helper()
	oldPoll, oldBackstop, oldResting := followPoll, followBackstop, followResting
	followPoll, followBackstop, followResting = poll, backstop, resting
	t.Cleanup(func() {
		followPoll, followBackstop, followResting = oldPoll, oldBackstop, oldResting
	})
}

// readerOnly hides the recorder's StepWaiter, so Follow must poll.
type readerOnly struct{ r StepReader }

func (r readerOnly) StepsAfter(
	ctx context.Context,
	turnID string,
	after int64,
	limit int,
) ([]Step, error) {
	return r.r.StepsAfter(ctx, turnID, after, limit)
}

// collect follows a turn on its own goroutine and returns the result later.
func collect(
	ctx context.Context,
	r StepReader,
	turnID string,
	after int64,
) func(t *testing.T) ([]Step, error) {
	done := make(chan struct{})
	var steps []Step
	var err error
	go func() {
		defer close(done)
		for step, e := range Follow(ctx, r, turnID, after) {
			if e != nil {
				err = e
				return
			}
			steps = append(steps, step)
		}
	}()
	return func(t *testing.T) ([]Step, error) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("follow did not end")
		}
		return steps, err
	}
}

func requireFollowed(t *testing.T, got []Step, first int64, turn Turn) {
	t.Helper()
	want := turn.Steps[first-1:]
	if len(got) != len(want) {
		t.Fatalf("followed %d steps from seq %d; want %d", len(got), first, len(want))
	}
	for i, step := range got {
		if !step.Equal(want[i]) {
			t.Fatalf("followed step %d is seq %d %s; want seq %d", i, step.Seq, step.Kind, want[i].Seq)
		}
	}
}

// pausedTurn records an interrupted turn and returns the runner that
// finishes it.
func pausedTurn(t *testing.T, rec *MemoryRecorder) (RunResult, Runner) {
	t.Helper()
	runner := Runner{
		Client:   &scriptedClient{results: []llm.Result{final("done")}},
		Recorder: rec,
	}
	pause := make(chan struct{})
	close(pause)
	first, err := runner.Run(t.Context(), Turn{ID: "t1"}, limitsStart(testAgent()), WithPause(pause))
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	return first, runner
}

// Two followers with different cursors read the record so far, then follow
// live, and both end at the stop that ends the turn.
func TestFollowReplaysThenFollowsLive(t *testing.T) {
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	fromStart := collect(t.Context(), rec, "t1", 0)
	fromTwo := collect(t.Context(), rec, "t1", 2)
	time.Sleep(20 * time.Millisecond)
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || !out.Stop.Ended {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	got, err := fromStart(t)
	if err != nil {
		t.Fatal(err)
	}
	requireFollowed(t, got, 1, out.Turn)
	got, err = fromTwo(t)
	if err != nil {
		t.Fatal(err)
	}
	requireFollowed(t, got, 3, out.Turn)
}

// A follower that leaves does not affect the turn.
func TestFollowerLeavingDoesNotCancel(t *testing.T) {
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for step, err := range Follow(ctx, rec, "t1", 0) {
		if err != nil {
			t.Fatal(err)
		}
		if step.Seq == 1 {
			cancel()
			break
		}
	}
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	// A follow whose context has ended yields nothing and no error.
	for step, err := range Follow(ctx, rec, "t1", 0) {
		t.Fatalf("canceled follow yielded %+v, %v", step, err)
	}
}

// Follow ends at the ending stop; a late result recorded after it is read
// with StepsAfter from the stop's Seq, as an accounting reader would.
func TestLateResultAfterEndedStopIsReadable(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote")}}
	rec := NewMemoryRecorder()
	panics := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		panic("boom")
	})
	runner := toolRunner(client, rec, panics)
	first, err := startTurn(testAgent(), runner)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	stopSeq := int64(len(canceled.Turn.Steps))
	got, err := collect(t.Context(), rec, "t1", 0)(t)
	if err != nil || int64(len(got)) != stopSeq {
		t.Fatalf("followed %d steps to the stop at %d: err=%v", len(got), stopSeq, err)
	}
	if _, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID: "late",
		CallID:    "call-1",
		Outcome:   new(tool.Succeeded("done late")),
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := collect(t.Context(), rec, "t1", stopSeq)(t); err != nil || len(got) != 0 {
		t.Fatalf("follow from an ended turn's stop: steps=%d err=%v", len(got), err)
	}
	late, err := rec.StepsAfter(t.Context(), "t1", stopSeq, 10)
	if err != nil || len(late) != 1 || late[0].Kind != StepToolFinished {
		t.Fatalf("late steps=%+v err=%v", late, err)
	}
}

// Follow reads every page before it waits: with a poll of an hour, a
// finished turn longer than one page still arrives at once.
func TestFollowDrainsBeforeWaiting(t *testing.T) {
	setFollowIntervals(t, time.Hour, time.Hour, time.Hour)
	old := followPage
	followPage = 2
	t.Cleanup(func() { followPage = old })
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || len(out.Turn.Steps) <= 2*followPage {
		t.Fatalf("resume: steps=%d err=%v", len(out.Turn.Steps), err)
	}
	got, err := collect(t.Context(), readerOnly{rec}, "t1", -5)(t)
	if err != nil {
		t.Fatal(err)
	}
	requireFollowed(t, got, 1, out.Turn)
}

// Without a StepWaiter, Follow polls for new steps.
func TestFollowPollsWithoutWaiter(t *testing.T) {
	// The paused turn rests at a stop, so the resting interval applies.
	setFollowIntervals(t, 10*time.Millisecond, time.Hour, 10*time.Millisecond)
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	follow := collect(t.Context(), readerOnly{rec}, "t1", 0)
	time.Sleep(30 * time.Millisecond)
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || !out.Stop.Ended {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	got, err := follow(t)
	if err != nil {
		t.Fatal(err)
	}
	requireFollowed(t, got, 1, out.Turn)
}

// failingWaiter's Wait always fails; Follow falls back to polling instead of
// spinning on it.
type failingWaiter struct {
	*MemoryRecorder
	calls atomic.Int64
}

func (w *failingWaiter) Wait(context.Context, string, int64) error {
	w.calls.Add(1)
	return errors.New("wake channel down")
}

func TestFollowFallsBackWhenWaitFails(t *testing.T) {
	setFollowIntervals(t, 20*time.Millisecond, time.Hour, 20*time.Millisecond)
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	w := &failingWaiter{MemoryRecorder: rec}
	follow := collect(t.Context(), w, "t1", 0)
	time.Sleep(200 * time.Millisecond)
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := follow(t)
	if err != nil {
		t.Fatal(err)
	}
	requireFollowed(t, got, 1, out.Turn)
	if n := w.calls.Load(); n > 20 {
		t.Fatalf("Wait called %d times in about 200ms; Follow spun on it", n)
	}
}

// scriptedReader returns fixed pages, then an error.
type scriptedReader struct {
	mu    sync.Mutex
	pages [][]Step
	err   error
}

func (r *scriptedReader) StepsAfter(context.Context, string, int64, int) ([]Step, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pages) == 0 {
		return nil, r.err
	}
	page := r.pages[0]
	r.pages = r.pages[1:]
	return page, nil
}

func TestFollowRejectsDiscontinuousSteps(t *testing.T) {
	for name, page := range map[string][]Step{
		"gap":        {{TurnID: "t1", Seq: 1}, {TurnID: "t1", Seq: 3}},
		"other turn": {{TurnID: "t2", Seq: 1}},
		"repeat":     {{TurnID: "t1", Seq: 1}, {TurnID: "t1", Seq: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			r := &scriptedReader{pages: [][]Step{page}}
			var errs []error
			for _, err := range Follow(t.Context(), r, "t1", 0) {
				if err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) != 1 || !errors.Is(errs[0], ErrRecordInvalid) {
				t.Fatalf("errs=%v", errs)
			}
		})
	}
}

// A read error, such as a cursor from another turn, is yielded once.
func TestFollowYieldsReadErrorOnce(t *testing.T) {
	rec := NewMemoryRecorder()
	pausedTurn(t, rec)
	var errs []error
	for _, err := range Follow(t.Context(), rec, "t1", 99) {
		errs = append(errs, err)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrCursorAhead) {
		t.Fatalf("errs=%v", errs)
	}
}

func TestFollowPollsSlowlyWhileResting(t *testing.T) {
	if got := pollInterval(followPoll, false); got != followPoll {
		t.Fatalf("poll=%v", got)
	}
	if got := pollInterval(followPoll, true); got != followResting {
		t.Fatalf("resting poll=%v, want %v", got, followResting)
	}
}

// Finishes names what a step replaces in a real run: model_call_started
// carries the deltas' MessageID but finishes nothing.
func TestStepFinishes(t *testing.T) {
	ok := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Succeeded("done"), nil
	})
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
		deltas:  []llm.Delta{{Kind: llm.DeltaText, Text: "hi"}},
	}
	runner := toolRunner(client, NewMemoryRecorder(), ok)
	deltaMessages := map[string]bool{}
	var steps []Step
	for event, err := range runner.Stream(t.Context(), Turn{ID: "t1"}, limitsStart(testAgent())) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil {
			deltaMessages[event.MessageID] = true
		}
		if event.Step != nil {
			steps = append(steps, *event.Step)
		}
	}
	finished := map[string]bool{}
	for _, step := range steps {
		id := step.Finishes()
		switch step.Kind {
		case StepModelCallStarted:
			if !deltaMessages[step.Model.MessageID] {
				t.Fatalf("intent message %q has no deltas", step.Model.MessageID)
			}
			if id != "" {
				t.Fatalf("model_call_started settles %q", id)
			}
		case StepModelCallFinished:
			if id != step.Model.MessageID || !deltaMessages[id] {
				t.Fatalf("model_call_finished settles %q; want its message", id)
			}
		case StepToolFinished:
			if id != "call-1" {
				t.Fatalf("tool_finished settles %q; want call-1", id)
			}
		default:
			if id != "" {
				t.Fatalf("%s settles %q", step.Kind, id)
			}
		}
		if id != "" {
			finished[id] = true
		}
	}
	if len(finished) != len(deltaMessages)+1 {
		t.Fatalf("settled=%v deltas=%v", finished, deltaMessages)
	}
}

// A reader whose cursor is already the ending stop ends at once.
func TestFollowFromEndedCursorEndsAtOnce(t *testing.T) {
	setFollowIntervals(t, time.Hour, time.Hour, time.Hour)
	rec := NewMemoryRecorder()
	first, runner := pausedTurn(t, rec)
	out, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || !out.Stop.Ended {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	last := int64(len(out.Turn.Steps))
	got, err := collect(t.Context(), rec, "t1", last)(t)
	if err != nil || len(got) != 0 {
		t.Fatalf("steps=%d err=%v", len(got), err)
	}
	// One past the end is ahead.
	_, err = collect(t.Context(), rec, "t1", last+1)(t)
	if !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("err=%v", err)
	}
}

// eagerWaiter's Wait always reports a step, which is wrong while the turn
// rests; Follow must not spin on it.
type eagerWaiter struct {
	*MemoryRecorder
	reads atomic.Int64
}

func (w *eagerWaiter) StepsAfter(
	ctx context.Context,
	turnID string,
	after int64,
	limit int,
) ([]Step, error) {
	w.reads.Add(1)
	return w.MemoryRecorder.StepsAfter(ctx, turnID, after, limit)
}

func (w *eagerWaiter) Wait(context.Context, string, int64) error {
	return nil
}

func TestFollowBacksOffAnEagerWaiter(t *testing.T) {
	rec := NewMemoryRecorder()
	pausedTurn(t, rec)
	w := &eagerWaiter{MemoryRecorder: rec}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	for _, err := range Follow(ctx, w, "t1", 0) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := w.reads.Load(); n > 10 {
		t.Fatalf("%d reads in 200ms; Follow spun on a waiter that woke falsely", n)
	}
}

func TestMemoryRecorderWaitsForCommit(t *testing.T) {
	rec := NewMemoryRecorder()
	pausedTurn(t, rec)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := rec.Wait(ctx, "t1", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if err := rec.Wait(t.Context(), "t1", 0); err != nil {
		t.Fatalf("err=%v", err)
	}
}
