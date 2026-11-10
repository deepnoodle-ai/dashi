package delegate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
)

// watchSteps runs Watch on its own goroutine and sends each update.
func watchSteps(ctx context.Context, ts *delegate.Tasks, id string) (<-chan delegate.TaskUpdate, <-chan error) {
	updates := make(chan delegate.TaskUpdate, 1024)
	done := make(chan error, 1)
	go func() {
		defer close(updates)
		for u, err := range ts.Watch(ctx, id) {
			if err != nil {
				done <- err
				return
			}
			updates <- u
		}
		done <- nil
	}()
	return updates, done
}

// collectUntilStop reads updates until the Turn's stop step and returns the
// Seq of every step it saw, from the record and from live events.
func collectUntilStop(t *testing.T, updates <-chan delegate.TaskUpdate) []int64 {
	t.Helper()
	var seqs []int64
	timeout := time.After(5 * time.Second)
	for {
		select {
		case u, ok := <-updates:
			if !ok {
				t.Fatalf("watch ended early after %v", seqs)
			}
			switch u.Kind {
			case delegate.TaskUpdateTurn:
				for _, s := range u.Turn.Steps {
					seqs = append(seqs, s.Seq)
					if s.Kind == dashi.StepTurnStopped {
						return seqs
					}
				}
			case delegate.TaskUpdateEvent:
				if s := u.Event.Step; s != nil {
					seqs = append(seqs, s.Seq)
					if s.Kind == dashi.StepTurnStopped {
						return seqs
					}
				}
			case delegate.TaskUpdateMissingSteps:
				t.Fatalf("missing: %v", u.Err)
			}
		case <-timeout:
			t.Fatalf("no stop step after %v", seqs)
		}
	}
}

func checkSeqs(t *testing.T, seqs []int64) {
	t.Helper()
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("steps %v are not 1..n once each", seqs)
		}
	}
}

func TestWatchStartedBeforeTheTaskEndsYieldsEachStepOnce(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "found it")), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	ctx, cancel := context.WithCancel(t.Context())
	updates, done := watchSteps(ctx, h.tasks, taskID())
	g.open()
	checkSeqs(t, collectUntilStop(t, updates))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a canceled watch ended with %v", err)
	}
}

func TestWatchStartedAfterTheTaskEndsReplaysTheRecord(t *testing.T) {
	h := newHarness(t, script("found it"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	ctx, cancel := context.WithCancel(t.Context())
	updates, done := watchSteps(ctx, h.tasks, taskID())
	u := <-updates
	if live := <-updates; live.Kind != delegate.TaskUpdateLive {
		t.Fatalf("second update=%+v", live)
	}
	if u.Kind != delegate.TaskUpdateTurn || u.Turn.ID != childTurnID() {
		t.Fatalf("first update=%+v", u)
	}
	var seqs []int64
	for _, s := range u.Turn.Steps {
		seqs = append(seqs, s.Seq)
	}
	checkSeqs(t, seqs)
	if last := u.Turn.Steps[len(seqs)-1]; last.Kind != dashi.StepTurnStopped {
		t.Fatalf("the record ends at %s", last.Kind)
	}
	// The watch stays open for a later turn until canceled.
	select {
	case u := <-updates:
		t.Fatalf("an ended task yielded %+v", u)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWatchOfAnUnknownTask(t *testing.T) {
	h := newHarness(t, script("found it"), delegate.Config{}, nil)
	for _, err := range h.tasks.Watch(t.Context(), "t_nope") {
		if !errors.Is(err, delegate.ErrUnknownTask) {
			t.Fatalf("err=%v", err)
		}
		return
	}
	t.Fatal("Watch yielded nothing")
}

func TestCloseEndsAWatch(t *testing.T) {
	h := newHarness(t, script("found it"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	updates, done := watchSteps(t.Context(), h.tasks, taskID())
	<-updates
	h.close(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the watch")
	}
}
