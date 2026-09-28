package dashi

import "sync"

// observerQueue bounds the events waiting for a slow Observer.
const observerQueue = 64

// Display events are bounded and lossy. The recorder, not Observer, is the
// authoritative path for committed steps. A broken observer cannot block work.
//
// Tool progress is the lowest priority: a progress event is queued only while
// the queue is under half full, and any other event arriving at a full queue
// evicts the oldest queued progress event before it is dropped itself. So
// progress never costs the Observer a step or delta that would otherwise fit.
type emitter struct {
	mu     sync.Mutex
	ready  *sync.Cond
	queue  []Event
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

// send queues event, dropping it when the queue is full and holds no
// progress event to evict.
func (e *emitter) send(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if len(e.queue) >= observerQueue {
		i := e.oldestProgress()
		if i < 0 {
			return
		}
		e.queue = append(e.queue[:i], e.queue[i+1:]...)
	}
	e.queue = append(e.queue, event)
	e.ready.Signal()
}

// sendProgress queues a tool progress event only while the queue is under
// half full.
func (e *emitter) sendProgress(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || len(e.queue) >= observerQueue/2 {
		return
	}
	e.queue = append(e.queue, event)
	e.ready.Signal()
}

// close lets the observer goroutine deliver what is queued and exit.
func (e *emitter) close() {
	if e == nil {
		return
	}
	e.mu.Lock()
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
