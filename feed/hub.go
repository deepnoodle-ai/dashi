// Package feed delivers a turn to live readers: committed steps from the
// record, merged with the live previews (model deltas and tool progress)
// that a Runner in this process produces. A web handler serves one Follow
// per connection; a Hub set as Runner.Observer supplies the previews.
//
// The rules Follow keeps for each reader:
//
//   - Steps are never dropped. They come from the record through
//     dashi.Follow, exactly once and in Seq order.
//   - A preview appears only in its place: after the step committed just
//     before it (Event.Seq) and before the next step. A preview whose place
//     the reader has passed is dropped, since a later step supersedes it.
//   - When deltas for a message are lost, whether a reader fell behind, the
//     Observer dropped them, or the reader joined while the message was
//     streaming, the reader gets one Event with Gap set for that message,
//     and no further deltas for it. Its settling step usually follows; if
//     the run ended early, as after a failed commit, reload the turn.
//   - Tool progress may be dropped without a gap: each snapshot replaces the
//     last. A reader that joins while a tool runs sees no progress for it
//     until the tool reports again.
//   - Observing never blocks the runner, and a slow reader never slows
//     another.
package feed

import (
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

// queueLimit bounds the previews queued in the hub for one reader, and
// separately those Follow holds until their step arrives. Gap markers do not
// count against either; each message has at most one.
const queueLimit = 256

// openTimeout forgets a turn nobody follows once it has been quiet this
// long: a message whose deltas stopped without a settling step, as when its
// run died, or the settled messages it remembers.
const openTimeout = 10 * time.Minute

// settledLimit bounds the settled messages a turn remembers. A delayed
// delta comes from a recent message, so the latest few suffice.
const settledLimit = 64

// Hub fans a Runner's previews out to the readers following its turns in
// this process. Set Runner.Observer to hub.Observe and pass the hub to
// Follow. The zero value is ready to use, and a nil *Hub observes nothing.
// A Hub must not be copied after first use.
//
// Steps reach the Hub too, but only as hints: they wake readers waiting for
// the record and mark a streaming message settled. Readers always take steps
// from the record. The Hub keeps one small entry per turn with readers, a
// message streaming, or recently settled messages (so a delayed delta from
// an overlapping run cannot reopen one); a terminal stop, or ten quiet
// minutes with no readers, removes it.
type Hub struct {
	mu        sync.Mutex
	turns     map[string]*turnState
	lastSweep time.Time
}

// turnState is one turn's readers and streaming message. Hub.mu guards the
// map and dead; mu guards the rest. Lock Hub.mu before mu.
type turnState struct {
	mu      sync.Mutex
	dead    bool
	readers map[*reader]struct{}
	// open is the message streaming now, so a reader that joins mid-message
	// starts with a gap for it.
	open *openMessage
	// stepSeq is the highest step Seq observed; wake is closed and replaced
	// when it grows.
	stepSeq int64
	wake    chan struct{}
	// settled holds messages whose settling step was observed, oldest
	// first in settledOrder, so a delayed delta or gap from an overlapping
	// run cannot reopen one.
	settled      map[string]bool
	settledOrder []string
	// last is when the turn last had an event, for the sweep.
	last time.Time
}

type openMessage struct {
	attemptID string
	messageID string
	seq       int64
	last      time.Time
}

// reader is one Follow's queue of previews and gaps, in arrival order.
type reader struct {
	queue    []dashi.Event
	previews int
	// lost records messages that lost deltas; their later deltas are
	// dropped and at most one gap is queued for each.
	lost  map[string]bool
	ready chan struct{}
}

// Observe is a dashi.Observer: set Runner.Observer = hub.Observe. It never
// blocks. Events are placed by their Seq, so a relay that feeds events
// from another process must keep Seq and gaps unchanged, and turn its own
// losses of deltas into gaps before calling Observe.
func (h *Hub) Observe(e dashi.Event) {
	if h == nil || e.TurnID == "" {
		return
	}
	switch {
	case e.Step != nil:
		h.observeStep(e)
	case e.Delta != nil:
		t := h.lock(e.TurnID, true)
		t.last = time.Now()
		if t.settled[e.MessageID] {
			t.mu.Unlock()
			return
		}
		t.streaming(e)
		for r := range t.readers {
			r.delta(e)
		}
		t.mu.Unlock()
	case e.Gap:
		// A gap also marks its message streaming, so a reader that joins
		// before its next delta starts with a gap for it.
		t := h.lock(e.TurnID, true)
		t.last = time.Now()
		if t.settled[e.MessageID] {
			t.mu.Unlock()
			return
		}
		t.streaming(e)
		for r := range t.readers {
			r.gap(e)
		}
		t.mu.Unlock()
	case e.Progress != nil:
		if t := h.lock(e.TurnID, false); t != nil {
			for r := range t.readers {
				r.progress(e)
			}
			t.mu.Unlock()
		}
	}
}

func (h *Hub) observeStep(e dashi.Event) {
	// A settled model message is remembered even for a turn nobody follows
	// yet, so a delayed delta cannot reopen it.
	settles := ""
	if e.Step.Model != nil {
		settles = e.Step.Settles()
	}
	t := h.lock(e.TurnID, settles != "")
	if t == nil {
		return
	}
	t.last = time.Now()
	if e.Step.Seq > t.stepSeq {
		t.stepSeq = e.Step.Seq
		close(t.wake)
		t.wake = make(chan struct{})
	}
	if settles != "" {
		t.markSettled(settles)
		if t.open != nil && t.open.messageID == settles {
			t.open = nil
		}
	}
	if e.Step.Stop != nil && e.Step.Stop.Terminal {
		// Nothing streams after the end; forget the turn's messages.
		t.open = nil
		clear(t.settled)
		t.settledOrder = nil
	}
	idle := t.idle()
	t.mu.Unlock()
	if idle {
		h.forget(e.TurnID)
	}
}

// streaming records e's message as the turn's open message. An event from
// an older run, placed before the open message, leaves it alone. The caller
// holds t.mu.
func (t *turnState) streaming(e dashi.Event) {
	if t.open != nil && e.Seq < t.open.seq {
		return
	}
	t.open = &openMessage{
		attemptID: e.ModelCallID,
		messageID: e.MessageID,
		seq:       e.Seq,
		last:      time.Now(),
	}
}

// lock returns the turn's state locked, creating it when create is set, or
// nil when it has none.
func (h *Hub) lock(turnID string, create bool) *turnState {
	for {
		h.mu.Lock()
		if h.turns == nil {
			h.turns = map[string]*turnState{}
		}
		t := h.turns[turnID]
		if t == nil && create {
			t = &turnState{
				readers: map[*reader]struct{}{},
				wake:    make(chan struct{}),
				settled: map[string]bool{},
				last:    time.Now(),
			}
			h.turns[turnID] = t
			h.sweep()
		}
		h.mu.Unlock()
		if t == nil {
			return nil
		}
		t.mu.Lock()
		if !t.dead {
			return t
		}
		t.mu.Unlock()
	}
}

// markSettled remembers a settled message, forgetting the oldest past
// settledLimit. The caller holds t.mu.
func (t *turnState) markSettled(id string) {
	if t.settled[id] {
		return
	}
	t.settled[id] = true
	t.settledOrder = append(t.settledOrder, id)
	if len(t.settledOrder) > settledLimit {
		delete(t.settled, t.settledOrder[0])
		t.settledOrder = t.settledOrder[1:]
	}
}

// idle reports whether the entry holds nothing worth keeping. The caller
// holds t.mu.
func (t *turnState) idle() bool {
	return len(t.readers) == 0 && t.open == nil && len(t.settled) == 0
}

// forget removes an idle turn's entry.
func (h *Hub) forget(turnID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.turns[turnID]
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idle() {
		t.dead = true
		delete(h.turns, turnID)
	}
}

// sweep drops, at most once a minute, entries nobody follows that have been
// quiet for openTimeout. The caller holds h.mu.
func (h *Hub) sweep() {
	now := time.Now()
	if now.Sub(h.lastSweep) < time.Minute {
		return
	}
	h.lastSweep = now
	for id, t := range h.turns {
		if !t.mu.TryLock() {
			continue
		}
		if len(t.readers) == 0 && now.Sub(t.last) > openTimeout {
			t.dead = true
			delete(h.turns, id)
		}
		t.mu.Unlock()
	}
}

// subscribe registers a reader for the turn. A message streaming now is
// already missing deltas for it, so the reader starts with a gap for it.
func (h *Hub) subscribe(turnID string) *reader {
	t := h.lock(turnID, true)
	defer t.mu.Unlock()
	r := &reader{lost: map[string]bool{}, ready: make(chan struct{}, 1)}
	t.readers[r] = struct{}{}
	if o := t.open; o != nil {
		r.gap(dashi.Event{
			TurnID:      turnID,
			ModelCallID: o.attemptID,
			MessageID:   o.messageID,
			Seq:         o.seq,
			Gap:         true,
		})
	}
	return r
}

func (h *Hub) unsubscribe(turnID string, r *reader) {
	t := h.lock(turnID, false)
	if t == nil {
		return
	}
	delete(t.readers, r)
	idle := t.idle()
	t.mu.Unlock()
	if idle {
		h.forget(turnID)
	}
}

// wakeAfter returns a channel that is closed once the Hub has observed a
// step after the given Seq for the turn. It never creates an entry: a
// reader's entry exists while it is registered, and without one it returns
// a closed channel so the caller reads the record.
func (h *Hub) wakeAfter(turnID string, after int64) <-chan struct{} {
	t := h.lock(turnID, false)
	if t == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	defer t.mu.Unlock()
	if t.stepSeq > after {
		done := make(chan struct{})
		close(done)
		return done
	}
	return t.wake
}

// entries reports the turns the hub holds, for tests.
func (h *Hub) entries() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.turns)
}

// readers reports the turn's readers, for tests.
func (h *Hub) readers(turnID string) int {
	t := h.lock(turnID, false)
	if t == nil {
		return 0
	}
	defer t.mu.Unlock()
	return len(t.readers)
}

// The reader methods run under the turn's lock.

func (r *reader) delta(e dashi.Event) {
	if r.lost[e.MessageID] {
		return
	}
	if r.previews >= queueLimit {
		r.gap(e)
		return
	}
	r.push(e, true)
}

func (r *reader) progress(e dashi.Event) {
	if r.previews < queueLimit {
		r.push(e, true)
	}
}

// gap queues one gap per message and drops its later deltas.
func (r *reader) gap(e dashi.Event) {
	if r.lost[e.MessageID] {
		return
	}
	r.lost[e.MessageID] = true
	r.push(dashi.Event{
		TurnID:      e.TurnID,
		ModelCallID: e.ModelCallID,
		MessageID:   e.MessageID,
		Seq:         e.Seq,
		Gap:         true,
	}, false)
}

func (r *reader) push(e dashi.Event, counted bool) {
	r.queue = append(r.queue, e)
	if counted {
		r.previews++
	}
	select {
	case r.ready <- struct{}{}:
	default:
	}
}

// take returns and clears the queue. The caller holds the turn's lock.
func (r *reader) take() []dashi.Event {
	q := r.queue
	r.queue, r.previews = nil, 0
	return q
}
