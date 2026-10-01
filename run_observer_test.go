package dashi

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// parkedEmitter returns an emitter whose Observer is parked on a first
// event until release is closed, and the events it later receives.
func parkedEmitter(t *testing.T) (*emitter, func() []Event) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var got []Event
	first := true
	e := newEmitter(func(event Event) {
		if first {
			first = false
			close(entered)
			<-release
			return
		}
		mu.Lock()
		got = append(got, event)
		mu.Unlock()
		if event.Step != nil && event.Step.Kind == StepStopped {
			close(done)
		}
	})
	e.send(Event{Step: &Step{Kind: StepContinued}})
	<-entered
	return e, func() []Event {
		close(release)
		e.send(Event{Step: &Step{Kind: StepStopped}})
		e.close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("observer did not drain")
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func delta(i int) Event {
	return Event{
		TurnID:    "t1",
		AttemptID: "t1:model:1",
		MessageID: "t1:model:1:message",
		Delta:     &llm.Delta{Kind: llm.DeltaText, Text: string(rune('a' + i%26))},
	}
}

func stepEvent(seq int64) Event {
	return Event{TurnID: "t1", Step: &Step{Seq: seq, Kind: StepDelivered}}
}

// kinds summarizes events as "d" (delta), "p" (progress), "g" (gap), or
// "s<seq>" (step), ignoring the final stop that parkedEmitter sends.
func kinds(events []Event) []string {
	var out []string
	for _, e := range events {
		switch {
		case e.Gap:
			out = append(out, "g")
		case e.Progress != nil:
			out = append(out, "p")
		case e.Delta != nil:
			out = append(out, "d")
		case e.Step != nil && e.Step.Kind != StepStopped:
			out = append(out, "s"+string(rune('0'+e.Step.Seq)))
		}
	}
	return out
}

func TestObserverKeepsEveryStep(t *testing.T) {
	e, drain := parkedEmitter(t)
	const steps = 2000
	for i := range steps {
		for j := range 5 {
			e.send(delta(j))
		}
		e.send(Event{TurnID: "t1", Step: &Step{Seq: int64(i + 1)}})
	}
	var seqs []int64
	gaps := 0
	for _, event := range drain() {
		if event.Gap {
			gaps++
			if event.TurnID != "t1" {
				t.Fatalf("gap turn=%q", event.TurnID)
			}
		}
		if event.Step != nil && event.Step.Kind != StepStopped {
			seqs = append(seqs, event.Step.Seq)
		}
	}
	if len(seqs) != steps {
		t.Fatalf("steps=%d, want %d", len(seqs), steps)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("step %d has seq %d", i, seq)
		}
	}
	if gaps == 0 {
		t.Fatal("dropped deltas made no gap")
	}
}

// A gap marker goes where the drop happened: after the previews queued
// before it, once per run of drops, before the next event enqueued.
func TestObserverGapFollowsQueuedPreviews(t *testing.T) {
	e, drain := parkedEmitter(t)
	for i := range observerQueue + 10 {
		e.send(delta(i))
	}
	e.send(stepEvent(1))
	// Steps take no preview slot, so this delta is dropped as well, and
	// its gap precedes the stop that drain sends.
	e.send(delta(0))
	got := kinds(drain())
	want := slices.Repeat([]string{"d"}, observerQueue)
	want = append(want, "g", "s1", "g")
	if !slices.Equal(got, want) {
		t.Fatalf("events=%v\nwant %v", got, want)
	}
}

// The gap names the message that lost deltas. Progress refused by a full
// queue neither flushes nor adds a gap.
func TestObserverGapNamesMessage(t *testing.T) {
	e, drain := parkedEmitter(t)
	for i := range observerQueue + 1 {
		e.send(delta(i))
	}
	e.sendProgress(Event{TurnID: "t1", Progress: &ToolProgress{CallID: "c"}})
	events := drain()
	got := kinds(events)
	want := append(slices.Repeat([]string{"d"}, observerQueue), "g")
	if !slices.Equal(got, want) {
		t.Fatalf("events=%v\nwant %v", got, want)
	}
	gap := events[observerQueue]
	if gap.TurnID != "t1" || gap.AttemptID != "t1:model:1" ||
		gap.MessageID != "t1:model:1:message" || gap.Delta != nil {
		t.Fatalf("gap=%+v", gap)
	}
}

// Progress can be queued once the queue drains below half, and a pending
// gap still precedes it.
func TestObserverGapPrecedesLaterProgress(t *testing.T) {
	e := &emitter{}
	e.ready = sync.NewCond(&e.mu)
	for i := range observerQueue + 1 {
		e.send(delta(i))
	}
	for range observerQueue {
		if _, ok := e.next(); !ok {
			t.Fatal("queue closed early")
		}
	}
	e.sendProgress(Event{TurnID: "t1", Progress: &ToolProgress{CallID: "c"}})
	gap, _ := e.next()
	progress, _ := e.next()
	if !gap.Gap || progress.Progress == nil {
		t.Fatalf("gap=%+v progress=%+v", gap, progress)
	}
}

// An Observer that panics keeps receiving later events.
func TestPanickingObserverKeepsReceiving(t *testing.T) {
	got := make(chan int64, 3)
	e := newEmitter(func(event Event) {
		got <- event.Step.Seq
		if event.Step.Seq == 1 {
			panic("observer bug")
		}
	})
	e.send(stepEvent(1))
	e.send(stepEvent(2))
	e.close()
	for want := int64(1); want <= 2; want++ {
		select {
		case seq := <-got:
			if seq != want {
				t.Fatalf("seq=%d, want %d", seq, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("observer stopped after a panic at seq %d", want-1)
		}
	}
}

func TestObserverGapAtClose(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	done := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	first := true
	e := newEmitter(func(event Event) {
		if first {
			first = false
			close(entered)
			<-release
			return
		}
		mu.Lock()
		got = append(got, event)
		n := len(got)
		mu.Unlock()
		if n == observerQueue+1 {
			close(done)
		}
	})
	e.send(stepEvent(1))
	<-entered
	for i := range observerQueue + 1 {
		e.send(delta(i))
	}
	e.close()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not drain")
	}
	mu.Lock()
	defer mu.Unlock()
	if last := got[len(got)-1]; !last.Gap || last.TurnID != "t1" {
		t.Fatalf("last event=%+v, want a gap for t1", last)
	}
}

// A delta evicting queued progress drops nothing it must report.
func TestObserverProgressEvictionMakesNoGap(t *testing.T) {
	e, drain := parkedEmitter(t)
	for range observerQueue {
		e.sendProgress(Event{TurnID: "t1", Progress: &ToolProgress{CallID: "c"}})
	}
	if n := e.len(); n != observerQueue/2 {
		t.Fatalf("progress queued=%d, want %d", n, observerQueue/2)
	}
	for i := range observerQueue {
		e.send(delta(i))
	}
	for _, k := range kinds(drain()) {
		if k == "g" {
			t.Fatal("evicting progress made a gap")
		}
	}
}

// A slow Observer receives every step of back-to-back invocations of one
// turn, each invocation's steps in order, even after deltas were dropped.
func TestSlowObserverGetsEveryStepAcrossInvocations(t *testing.T) {
	deltas := make([]llm.Delta, 500)
	for i := range deltas {
		deltas[i] = llm.Delta{Kind: llm.DeltaText, Text: "x"}
	}
	client := &scriptedClient{results: []llm.Result{final("done")}, deltas: deltas}
	release := make(chan struct{})
	var mu sync.Mutex
	var seqs []int64
	gaps := 0
	stopped := make(chan struct{}, 2)
	runner := Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
		Observer: func(event Event) {
			<-release
			mu.Lock()
			defer mu.Unlock()
			if event.Gap {
				gaps++
			}
			if event.Step != nil {
				seqs = append(seqs, event.Step.Seq)
				if event.Step.Kind == StepStopped {
					stopped <- struct{}{}
				}
			}
		},
	}
	pause := make(chan struct{})
	close(pause)
	first, err := runner.Run(t.Context(), Turn{ID: "t1"}, limitsStart(testAgent()), WithPause(pause))
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("first: stop=%+v err=%v", first.Stop, err)
	}
	second, err := runner.Run(t.Context(), first.Turn, Continue{CommandID: "resume"})
	if err != nil || second.Stop.Kind != StopCompleted {
		t.Fatalf("second: stop=%+v err=%v", second.Stop, err)
	}
	close(release)
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("observer missed a stop")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(seqs)
	if len(seqs) != len(second.Turn.Steps) {
		t.Fatalf("observed %d steps, recorded %d", len(seqs), len(second.Turn.Steps))
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("observed seqs %v", seqs)
		}
	}
	if gaps != 1 {
		t.Fatalf("gaps=%d, want 1", gaps)
	}
}
