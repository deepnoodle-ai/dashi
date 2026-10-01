package dashi

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
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
		if event.Step != nil && event.Step.Kind == StepTurnStopped {
			close(done)
		}
	})
	e.send(Event{Step: &Step{Kind: StepTurnContinued}})
	<-entered
	return e, func() []Event {
		close(release)
		e.send(Event{Step: &Step{Kind: StepTurnStopped}})
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
		TurnID:      "t1",
		ModelCallID: "t1:model:1",
		MessageID:   "t1:model:1:message",
		Delta:       &llm.Delta{Kind: llm.DeltaText, Text: string(rune('a' + i%26))},
	}
}

func stepEvent(seq int64) Event {
	return Event{TurnID: "t1", Step: &Step{Seq: seq, Kind: StepMessagesAdded}}
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
		case e.Step != nil && e.Step.Kind != StepTurnStopped:
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
		if event.Step != nil && event.Step.Kind != StepTurnStopped {
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
	if gap.TurnID != "t1" || gap.ModelCallID != "t1:model:1" ||
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
				if event.Step.Kind == StepTurnStopped {
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

// loseSettle fails the n-th model_call_finished commit as a lost write, so the
// record ends at its model_call_started step, as after a crash.
type loseSettle struct {
	*MemoryRecorder
	n, seen int
}

func (r *loseSettle) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelCallFinished {
		r.seen++
		if r.seen == r.n {
			return &CommitError{Err: errors.New("lost")}
		}
	}
	return r.MemoryRecorder.Commit(ctx, req)
}

// Event.Seq places each preview after the step it follows: a delta at its
// message's model_call_started step, a progress snapshot between its call's
// tool_started and tool_finished steps. This holds only while commit and
// emit share the run goroutine; a change that emits from a tool goroutine
// or commits from another goroutine breaks it.
func TestEventSeqPlacesPreviews(t *testing.T) {
	work := progressTool("work", func(ctx context.Context, call tool.Call) (tool.Outcome, error) {
		for i := range 3 {
			tool.Report(ctx, tool.Progress{Message: call.ID, Done: float64(i)})
		}
		return tool.Succeeded("ok"), nil
	})
	client := &scriptedClient{
		results: []llm.Result{
			namedCallsResult("work", "work", "work"),
			final("lost"), // its settle is lost, so the next run resends it
			final("done"),
		},
		deltas: []llm.Delta{{Kind: llm.DeltaText, Text: "a"}, {Kind: llm.DeltaText, Text: "b"}},
	}
	mem := NewMemoryRecorder()
	agent := testAgent()
	agent.Limits.ToolConcurrency = 3
	runner := Runner{
		Client:   client,
		Tools:    tool.List{work},
		Recorder: &loseSettle{MemoryRecorder: mem, n: 2},
	}
	var events []Event
	for e, err := range runner.Stream(t.Context(), Turn{ID: "t1"}, limitsStart(agent)) {
		if e.Result == nil && err == nil {
			events = append(events, e)
		}
	}
	saved, err := mem.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if last := saved.Steps[len(saved.Steps)-1]; last.Kind != StepModelCallStarted {
		t.Fatalf("record ends at %s; want a model_call_started to recover", last.Kind)
	}
	for e, err := range runner.Stream(t.Context(), saved, Continue{CommandID: "resume"}) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Result == nil {
			events = append(events, e)
		}
	}
	final, err := mem.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	requested := map[string]int64{}
	started := map[string]int64{}
	settled := map[string]int64{}
	for _, step := range final.Steps {
		switch step.Kind {
		case StepModelCallStarted:
			requested[step.Model.MessageID] = step.Seq
		case StepToolStarted:
			started[step.Tool.Call.ID] = step.Seq
		case StepToolFinished:
			settled[step.Tool.Call.ID] = step.Seq
		}
	}
	var deltas, progress int
	for _, e := range events {
		switch {
		case e.Step != nil:
			if e.Seq != e.Step.Seq {
				t.Fatalf("step %d carries Seq %d", e.Step.Seq, e.Seq)
			}
		case e.Delta != nil:
			deltas++
			if want, ok := requested[e.MessageID]; !ok || e.Seq != want {
				t.Fatalf("delta of %s at Seq %d; its model_call_started is %d", e.MessageID, e.Seq, want)
			}
		case e.Progress != nil:
			progress++
			id := e.Progress.CallID
			if e.Seq < started[id] || e.Seq >= settled[id] {
				t.Fatalf("progress of %s at Seq %d; want [%d, %d)", id, e.Seq, started[id], settled[id])
			}
		}
	}
	if deltas != 2*3 || progress == 0 { // three sends, two deltas each
		t.Fatalf("deltas=%d progress=%d", deltas, progress)
	}
}
