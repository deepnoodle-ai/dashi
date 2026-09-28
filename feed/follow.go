package feed

import (
	"context"
	"iter"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

// Follow yields the turn's committed steps after the cursor from r, as
// dashi.Follow does, with h's previews and gaps merged in place (see the
// package rules). Every event carries Seq: a step's own, or for a preview
// or gap the step it belongs after. A nil h yields steps only.
//
// It ends as dashi.Follow does: when ctx ends, after a terminal stop, or
// after yielding an error, such as one matching dashi.ErrCursorAhead. On any
// exit, including a break, it unsubscribes from h, stops its reader of the
// record, and drops previews it was holding.
//
// It registers with h before its first read of the record, so no preview
// falls between the two; a message already streaming then arrives as a gap.
// Steps the hub observes wake the read of the record, so previews are not
// held back by the reader's poll interval; r's own StepWaiter, if any, still
// applies, and without one the record is read at least every second.
//
// Each call starts one goroutine to read the record. Placement is exact
// because the Runner stamps Seq on the goroutine that commits.
func Follow(
	ctx context.Context,
	h *Hub,
	r dashi.StepReader,
	turnID string,
	after int64,
) iter.Seq2[dashi.Event, error] {
	return func(yield func(dashi.Event, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var rd *reader
		var ready <-chan struct{}
		source := r
		if h != nil {
			rd = h.subscribe(turnID)
			defer h.unsubscribe(turnID, rd)
			ready = rd.ready
			source = wakingReader{StepReader: r, hub: h}
		}
		type read struct {
			step dashi.Step
			err  error
		}
		steps := make(chan read)
		go func() {
			defer close(steps)
			for step, err := range dashi.Follow(ctx, source, turnID, after) {
				select {
				case steps <- read{step, err}:
				case <-ctx.Done():
					return
				}
			}
		}()

		m := merger{cursor: max(after, 0), lost: map[string]bool{}, yield: yield}
		for {
			select {
			case got, ok := <-steps:
				if !ok {
					return
				}
				if got.err != nil {
					yield(dashi.Event{}, got.err)
					return
				}
				step := got.step
				event := dashi.Event{
					TurnID: step.TurnID,
					Seq:    step.Seq,
					Step:   &step,
				}
				if step.Model != nil {
					event.AttemptID = step.Model.AttemptID
					event.MessageID = step.Model.MessageID
				}
				if !yield(event, nil) {
					return
				}
				if step.Stop != nil && step.Stop.Terminal {
					return
				}
				m.cursor = step.Seq
				// Previews that belong after this step must come out
				// before the next one, even within one page of steps:
				// first those held, which arrived earlier, then those
				// queued since.
				if !m.release() {
					return
				}
				if rd != nil && !m.place(h.take(turnID, rd)) {
					return
				}
			case <-ready:
				if !m.place(h.take(turnID, rd)) {
					return
				}
			}
		}
	}
}

// merger places previews against the last step yielded.
type merger struct {
	cursor int64
	held   []dashi.Event
	lost   map[string]bool
	yield  func(dashi.Event, error) bool
}

// place yields each event whose place is the cursor, holds those whose
// step has not been yielded yet, and drops the rest. It reports false when
// the consumer stopped.
func (m *merger) place(events []dashi.Event) bool {
	for _, e := range events {
		switch {
		case e.Seq <= 0 || e.Seq < m.cursor:
			// Unplaced, or superseded by a step already yielded.
		case e.Seq > m.cursor:
			m.hold(e)
		default:
			if !m.emit(e) {
				return false
			}
		}
	}
	return true
}

// hold keeps a preview until its step is yielded, within queueLimit.
func (m *merger) hold(e dashi.Event) {
	if e.Gap || len(m.held) < queueLimit {
		m.held = append(m.held, e)
		return
	}
	if e.Delta != nil && !m.lost[e.MessageID] {
		gap := e
		gap.Delta, gap.Gap = nil, true
		m.held = append(m.held, gap)
	}
}

// release yields held previews that belong at the cursor and drops those
// it has passed.
func (m *merger) release() bool {
	keep := m.held[:0]
	var now []dashi.Event
	for _, e := range m.held {
		switch {
		case e.Seq == m.cursor:
			now = append(now, e)
		case e.Seq > m.cursor:
			keep = append(keep, e)
		}
	}
	clear(m.held[len(keep):])
	m.held = keep
	for _, e := range now {
		if !m.emit(e) {
			return false
		}
	}
	return true
}

// emit yields one placed preview or gap, once per message for gaps, and
// never a delta of a message that lost deltas.
func (m *merger) emit(e dashi.Event) bool {
	switch {
	case e.Gap:
		if m.lost[e.MessageID] {
			return true
		}
		m.lost[e.MessageID] = true
	case e.Delta != nil && m.lost[e.MessageID]:
		return true
	}
	return m.yield(e, nil)
}

// take returns the reader's queued events.
func (h *Hub) take(turnID string, r *reader) []dashi.Event {
	t := h.lock(turnID, false)
	if t == nil {
		return nil
	}
	defer t.mu.Unlock()
	return r.take()
}

// followPoll bounds a wait when the reader has no StepWaiter of its own,
// matching dashi.Follow's poll. It is a variable so tests can shorten it.
var followPoll = time.Second

// wakingReader lets steps the Hub observes wake dashi.Follow.
type wakingReader struct {
	dashi.StepReader
	hub *Hub
}

func (w wakingReader) Wait(ctx context.Context, turnID string, after int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woken := w.hub.wakeAfter(turnID, after)
	inner := make(chan error, 1)
	var poll <-chan time.Time
	if waiter, ok := w.StepReader.(dashi.StepWaiter); ok {
		go func() { inner <- waiter.Wait(ctx, turnID, after) }()
	} else {
		timer := time.NewTimer(followPoll)
		defer timer.Stop()
		poll = timer.C
	}
	select {
	case <-woken:
		return nil
	case err := <-inner:
		return err
	case <-poll:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
