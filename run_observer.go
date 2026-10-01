package dashi

import (
	"sync"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Event carries one committed step, provisional model delta, tool progress
// snapshot, or gap marker, from Runner.Stream or to an Observer.
type Event struct {
	TurnID      string
	ModelCallID string
	MessageID   string
	// Seq places the event in the turn's record. On a step event it equals
	// Step.Seq. On a preview or gap it is the Seq of the last step committed
	// before it: the preview belongs after that step and is superseded by a
	// later one. A delta's Seq is its model_call_started step; a progress
	// snapshot's lies between its tool_started and tool_finished steps. A
	// relay that carries events between processes keeps Seq unchanged.
	Seq   int64
	Step  *Step
	Delta *llm.Delta
	// Progress is the latest tool.Report snapshot of a running tool call. It
	// is a preview, like Delta: never recorded and never replayed.
	Progress *ToolProgress
	// Gap marks model deltas the Observer dropped because it fell behind.
	// ModelCallID and MessageID name the message that lost them. Its preview
	// now has a hole: discard it, and ignore further deltas for that
	// MessageID until the step that settles it (model_call_finished) arrives. A
	// gap with no such step after it means the run ended early, as after a
	// failed commit; load the turn from the recorder. Dropped tool progress
	// makes no gap, since each snapshot replaces the last. Runner.Stream
	// never drops, so it never yields a gap.
	Gap bool
	// Outcome is set only on the final Runner.Stream event. Observer receives
	// steps, deltas, progress, and gaps, not this terminal event.
	Outcome *Outcome
}

// Observer receives events while a run is in progress. It runs on its own
// goroutine, so it never blocks the turn, and it must return promptly.
//
// Every step the recorder acknowledges reaches the Observer, in order.
// Previews (model deltas and tool progress) wait in a bounded queue and are
// dropped when it is full; dropped deltas are reported by one Event with Gap
// set. A step whose commit failed with unknown persistence is only in the
// record.
//
// Each Run or Stream call delivers on its own goroutine, which keeps
// draining after the call returns: Run returning does not mean the Observer
// has received every event. Events from one call arrive in order, but calls
// that overlap, such as a Continue issued before the previous call's events
// drain, may deliver concurrently; order steps by Seq.
//
// Steps are never dropped, so a blocked Observer holds its goroutine and a
// copy of every step it has not received, tool outputs and raw model results
// included, for each call it has fallen behind on, until it returns.
type Observer func(Event)

// observerQueue bounds the previews waiting for a slow Observer.
const observerQueue = 64

// emitter delivers events to an Observer without blocking the turn. Steps
// always enter its queue. Previews count against observerQueue: tool
// progress is queued only while fewer than half the slots hold previews,
// and a delta arriving at a full queue evicts the oldest queued progress,
// or else is dropped. A dropped delta makes the next event enqueued, or
// close, first enqueue one gap marker naming the delta's message.
//
// Eviction scans the queue, but only while progress is queued, and no new
// progress enters a full queue, so a backlog of steps is scanned at most
// once per evicted snapshot.
type emitter struct {
	mu    sync.Mutex
	ready *sync.Cond
	queue []Event
	// previews counts queued deltas and progress; progress counts the
	// progress among them, so a full queue with none skips the scan.
	previews int
	progress int
	// gap is the marker for dropped deltas not yet enqueued, or nil.
	gap    *Event
	closed bool
}

// newEmitter returns nil when there is no observer; a nil emitter is inactive.
func newEmitter(observer Observer) *emitter {
	if observer == nil {
		return nil
	}
	e := &emitter{queue: make([]Event, 0, observerQueue)}
	e.ready = sync.NewCond(&e.mu)
	go func() {
		for {
			event, ok := e.next()
			if !ok {
				return
			}
			func() { defer func() { _ = recover() }(); observer(event) }()
		}
	}()
	return e
}

// active reports whether an Observer is listening.
func (e *emitter) active() bool {
	return e != nil
}

// send queues a step, which is never dropped, or a delta, which is dropped
// when the previews fill the queue and none of them is progress to evict.
func (e *emitter) send(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if event.Step == nil && e.previews >= observerQueue {
		if e.progress == 0 {
			if e.gap == nil {
				e.gap = &Event{
					TurnID:      event.TurnID,
					ModelCallID: event.ModelCallID,
					MessageID:   event.MessageID,
					Seq:         event.Seq,
					Gap:         true,
				}
			}
			return
		}
		e.remove(e.oldestProgress())
	}
	e.enqueue(event)
}

// sendProgress queues a tool progress event only while previews fill less
// than half the queue.
func (e *emitter) sendProgress(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.previews >= observerQueue/2 {
		return
	}
	e.enqueue(event)
}

// enqueue appends event after any pending gap marker. The caller holds mu.
func (e *emitter) enqueue(event Event) {
	e.flushGap()
	e.queue = append(e.queue, event)
	e.count(event, 1)
	e.ready.Signal()
}

// remove deletes the queued event at i. The caller holds mu.
func (e *emitter) remove(i int) {
	e.count(e.queue[i], -1)
	copy(e.queue[i:], e.queue[i+1:])
	e.queue[len(e.queue)-1] = Event{}
	e.queue = e.queue[:len(e.queue)-1]
}

// count adjusts the preview counts by n for event. The caller holds mu.
func (e *emitter) count(event Event, n int) {
	if preview(event) {
		e.previews += n
	}
	if event.Progress != nil {
		e.progress += n
	}
}

// flushGap enqueues the pending gap marker, if any. The caller holds mu.
func (e *emitter) flushGap() {
	if e.gap == nil {
		return
	}
	e.queue = append(e.queue, *e.gap)
	e.gap = nil
}

// close reports any pending gap, then lets the observer goroutine deliver
// what is queued and exit.
func (e *emitter) close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.flushGap()
	e.closed = true
	e.mu.Unlock()
	e.ready.Broadcast()
}

// next blocks for the next queued event; ok is false once closed and empty.
func (e *emitter) next() (Event, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(e.queue) == 0 && !e.closed {
		e.ready.Wait()
	}
	if len(e.queue) == 0 {
		return Event{}, false
	}
	event := e.queue[0]
	e.count(event, -1)
	e.queue[0] = Event{}
	e.queue = e.queue[1:]
	return event, true
}

// len reports the queued events, for tests.
func (e *emitter) len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

func (e *emitter) oldestProgress() int {
	for i, event := range e.queue {
		if event.Progress != nil {
			return i
		}
	}
	return -1
}

// preview reports whether event counts against the queue bound.
func preview(event Event) bool {
	return event.Step == nil && !event.Gap
}
