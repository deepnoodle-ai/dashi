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
// It ends as dashi.Follow does: when ctx ends, after the stop that ends the
// turn, or after yielding an error, such as one matching
// dashi.ErrCursorAhead. On any exit, including a break, it stops its reader of the record and waits for
// it, unsubscribes from h, and drops previews it was holding.
//
// It registers with h before its first read of the record, so no preview
// falls between the two; a message already streaming then arrives as a gap.
// Steps the hub observes wake the read of the record, so previews are not
// held back by the reader's poll interval. r's own StepWaiter, if any, still
// applies; without one the record is also polled, every second, or every 30
// seconds while the turn rests at a stop, as dashi.Follow does.
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
			source = &wakingReader{StepReader: r, hub: h}
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
		// Stop and wait for the record's reader before unsubscribing, so it
		// cannot touch the hub for this turn afterwards.
		defer func() {
			cancel()
			for range steps {
			}
		}()

		m := merger{
			cursor:   max(after, 0),
			lost:     map[string]bool{},
			heldLost: map[string]bool{},
			yield:    yield,
		}
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
				// Previews queued before this step was read belong at or
				// before it: place them first. A delta the Observer
				// delivers after its finish step is dropped; the step
				// carries the whole message.
				if rd != nil && !m.place(h.take(turnID, rd)) {
					return
				}
				step := got.step
				event := dashi.Event{
					TurnID: step.TurnID,
					Seq:    step.Seq,
					Step:   &step,
				}
				if step.Model != nil {
					event.ModelCallID = step.Model.ModelCallID
					event.MessageID = step.Model.MessageID
				}
				if !yield(event, nil) {
					return
				}
				if step.Stop != nil && step.Stop.Ended {
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
	// lost holds messages whose gap was yielded; heldLost those whose gap is
	// held, so each message holds at most one gap and no later deltas.
	lost     map[string]bool
	heldLost map[string]bool
	yield    func(dashi.Event, error) bool
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

// hold keeps a preview until its step is yielded. Up to queueLimit
// previews are held; past that, progress is dropped and a delta becomes its
// message's gap. A message holds at most one gap, after which its deltas
// are dropped, so the held gaps are bounded by the messages.
func (m *merger) hold(e dashi.Event) {
	switch {
	case m.heldLost[e.MessageID] && (e.Gap || e.Delta != nil):
		return
	case e.Gap:
		m.heldLost[e.MessageID] = true
		m.held = append(m.held, e)
	case len(m.held) < queueLimit:
		m.held = append(m.held, e)
	case e.Delta != nil:
		m.heldLost[e.MessageID] = true
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

// followPoll and followResting bound a wait when the reader has no
// StepWaiter of its own, matching dashi.Follow's poll while the turn runs
// and while it rests at a stop. They are variables so tests can shorten
// them.
var (
	followPoll    = time.Second
	followResting = 30 * time.Second
)

// wakingReader lets steps the Hub observes wake dashi.Follow. dashi.Follow
// calls it from one goroutine, so its fields need no lock.
type wakingReader struct {
	dashi.StepReader
	hub *Hub
	// resting records that the last step read is a stop. dashi.Follow then
	// reads the record only every 30 seconds, and so does Wait.
	resting bool
}

func (w *wakingReader) StepsAfter(
	ctx context.Context,
	turnID string,
	after int64,
	limit int,
) ([]dashi.Step, error) {
	steps, err := w.StepReader.StepsAfter(ctx, turnID, after, limit)
	if len(steps) > 0 {
		w.resting = steps[len(steps)-1].Kind == dashi.StepTurnStopped
	}
	return steps, err
}

func (w *wakingReader) Wait(ctx context.Context, turnID string, after int64) error {
	ctx, cancel := context.WithCancel(ctx)
	woken := w.hub.wakeAfter(turnID, after)
	inner := make(chan error, 1)
	var poll <-chan time.Time
	if waiter, ok := w.StepReader.(dashi.StepWaiter); ok {
		done := make(chan struct{})
		go func() {
			defer close(done)
			inner <- waiter.Wait(ctx, turnID, after)
		}()
		// Follow promises to wait for its reader on exit, so stop the
		// inner Wait and wait for it.
		defer func() {
			cancel()
			<-done
		}()
	} else {
		// A commit from another process reaches no hub, so poll too.
		interval := followPoll
		if w.resting {
			interval = followResting
		}
		timer := time.NewTimer(interval)
		defer timer.Stop()
		poll = timer.C
	}
	defer cancel()
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
