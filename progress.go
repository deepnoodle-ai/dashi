package dashi

import (
	"encoding/json"
	"sync"

	"github.com/deepnoodle-ai/dashi/tool"
)

// ToolProgress is the latest snapshot a running tool reported with
// tool.Report. It is a preview: never recorded, never replayed, and never
// delivered after the call's tool_settled or tool_waiting step. Snapshots
// merge when the consumer is slower than the tool. While the turn runs,
// Runner.Stream delivers each call's latest snapshot before the call
// settles; Runner.Observer is lossy and may drop any snapshot, and a
// canceled turn delivers no more.
type ToolProgress struct {
	// CallID identifies the call, as on its tool_started step.
	CallID string

	// Name is the tool name, so a display can label the call.
	Name string

	tool.Progress
}

// progressHub delivers tool progress for one tools() pass. Tool goroutines
// write to their own slot and signal ready; only the runner goroutine reads
// slots and emits, so Stream's yield and the Observer queue are never touched
// from a tool goroutine and a slow consumer never blocks a tool.
type progressHub struct {
	ready chan struct{}
	// slots is owned by the runner goroutine.
	slots map[string]*progressSlot
}

// progressSlot holds the latest unsent snapshot of one call. A closed slot
// rejects new reports but keeps its last snapshot for delivery before settle.
type progressSlot struct {
	hub  *progressHub
	call tool.Call

	mu      sync.Mutex
	pending *tool.Progress
	closed  bool
}

// newProgressHub returns nil when no one listens, so calls get a silent
// reporter and nothing is buffered.
func newProgressHub(listening bool) *progressHub {
	if !listening {
		return nil
	}
	return &progressHub{
		ready: make(chan struct{}, 1),
		slots: map[string]*progressSlot{},
	}
}

// open registers a slot for call. It runs on the runner goroutine after the
// call's tool_started step is committed.
func (h *progressHub) open(call tool.Call) *progressSlot {
	if h == nil {
		return nil
	}
	slot := &progressSlot{hub: h, call: call}
	h.slots[call.ID] = slot
	return slot
}

// readyChan returns nil without a hub; a nil channel is never selected.
func (h *progressHub) readyChan() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.ready
}

// take removes and returns the pending snapshot of callID, forgetting the
// slot when forget is set.
func (h *progressHub) take(callID string, forget bool) *ToolProgress {
	if h == nil {
		return nil
	}
	slot := h.slots[callID]
	if slot == nil {
		return nil
	}
	if forget {
		delete(h.slots, callID)
	}
	slot.mu.Lock()
	p := slot.pending
	slot.pending = nil
	slot.mu.Unlock()
	if p == nil {
		return nil
	}
	return &ToolProgress{
		CallID:   slot.call.ID,
		Name:     slot.call.Name,
		Progress: *p,
	}
}

// reporter returns the function installed with tool.WithReporter for this
// call. A nil slot yields a silent reporter, which still shadows any reporter
// from an outer turn, so a nested runner's tools never report on the
// parent's call.
func (s *progressSlot) reporter() func(tool.Progress) {
	if s == nil {
		return nil
	}
	return s.report
}

func (s *progressSlot) report(p tool.Progress) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pending = &p
	s.mu.Unlock()
	select {
	case s.hub.ready <- struct{}{}:
	default:
	}
}

func (s *progressSlot) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// emitProgress sends every pending snapshot in call order. Snapshots are
// copied out under each slot's lock and emitted after it is released.
func (r *runtime) emitProgress(h *progressHub, order []string) {
	for _, id := range order {
		r.emitToolProgress(h.take(id, false))
	}
}

func (r *runtime) emitToolProgress(p *ToolProgress) {
	if p == nil {
		return
	}
	r.emit(Event{TurnID: r.turn.ID, Progress: p})
}

func cloneToolProgress(p *ToolProgress) *ToolProgress {
	c := *p
	if c.Data != nil {
		c.Data = append(json.RawMessage(nil), c.Data...)
	}
	return &c
}
