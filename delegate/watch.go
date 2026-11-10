package delegate

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/session"
)

// TaskUpdateKind says what a TaskUpdate holds.
type TaskUpdateKind string

const (
	// TaskUpdateTurn carries one child Turn as the record held it when the
	// watch started.
	TaskUpdateTurn TaskUpdateKind = "turn"
	// TaskUpdateEvent carries one event of a child Turn: a step, a model
	// delta, tool progress, or a gap.
	TaskUpdateEvent TaskUpdateKind = "event"
	// TaskUpdateRecordEnd marks the end of the record: every update after it
	// comes from live events. The watch yields it once.
	TaskUpdateRecordEnd TaskUpdateKind = "record_end"
	// TaskUpdateMissingSteps reports steps the watch could not get.
	TaskUpdateMissingSteps TaskUpdateKind = "missing_steps"
)

// TaskUpdate is one item that Tasks.Watch yields: a recorded Turn, a live
// child event, or a report of missing steps. It differs from TaskEvent,
// which reports that a task started, ended, or sent a message.
type TaskUpdate struct {
	Kind TaskUpdateKind `json:"kind"`
	// Turn is the recorded child Turn of a TaskUpdateTurn.
	Turn *dashi.Turn `json:"turn,omitempty"`
	// Event is the event of a TaskUpdateEvent. A step event follows the
	// last step of its Turn; a preview or gap belongs after the last step.
	Event dashi.Event `json:"event"`
	// TurnID names the Turn of a TaskUpdateMissingSteps that lost steps. It is
	// empty when the record failed to load when the watch started.
	TurnID string `json:"turn_id,omitempty"`
	// Err says why the steps of a TaskUpdateMissingSteps are missing.
	Err error `json:"-"`
}

// watchMissingWait is how long a watch waits for a missing step before it loads
// the Turn from the record.
const watchMissingWait = 100 * time.Millisecond

// watchLoadTimeout bounds one record load of a watch.
const watchLoadTimeout = 30 * time.Second

// watchPreviewLimit bounds the previews one watch holds. Past it, progress
// is dropped and a delta becomes a gap of its message. Steps are never
// dropped.
const watchPreviewLimit = 4096

// Watch yields the updates of task id in order. It first yields one
// TaskUpdateTurn for each child Turn the record holds and one
// TaskUpdateRecordEnd, then a TaskUpdateEvent for each live child event after them, as Config.OnChild
// receives them, with these rules:
//
//   - Watch listens before it loads the record, so no event falls between
//     the two. Events that arrive during the load wait for it.
//   - Each step comes once, in Seq order. A step the record or an earlier
//     event gave is dropped. Events of a later Turn wait while an earlier
//     Turn misses a step.
//   - A step that arrives after a missing one waits. When the missing step
//     has not arrived 100ms later, Watch loads that Turn from the record and
//     yields the steps it lacked. When the record lacks them too, or fails
//     to load, Watch yields a TaskUpdateMissingSteps and continues from the next
//     step it has.
//   - A preview (delta or progress) comes after the step it belongs after.
//     One that a later step superseded, or whose message or tool call has
//     finished, is dropped. A gap means its message lost deltas: discard
//     its preview, and Watch drops its later deltas.
//
// When the record fails to load at the start, Watch yields a
// TaskUpdateMissingSteps with an empty TurnID before TaskUpdateRecordEnd, and each Turn starts at the first
// step that arrives. It needs a child recorder that implements dashi.Loader
// to load the record; without one, every load fails this way.
//
// A task that ends keeps its watch: a later task_send starts a new Turn,
// whose events follow. Watch ends when ctx ends, when the loop breaks, or
// when Close runs, without an error; after Close it yields nothing. It yields ErrUnknownTask, and ends, for
// an ID that Tasks does not hold. Each call runs on the caller's goroutine
// and starts none; loads run there too.
func (ts *Tasks) Watch(ctx context.Context, id string) iter.Seq2[TaskUpdate, error] {
	return func(yield func(TaskUpdate, error) bool) {
		ts.mu.Lock()
		t, closed := ts.tasks[id], ts.closed
		ts.mu.Unlock()
		switch {
		case closed:
			return
		case t == nil:
			yield(TaskUpdate{}, ErrUnknownTask)
			return
		}
		var loader dashi.Loader
		if t.session != nil {
			loader, _ = t.session.Runner.Recorder.(dashi.Loader)
		}
		q := ts.listen(id)
		if q == nil {
			return
		}
		defer ts.unlisten(id, q)

		w := newWatchState()
		load := func(f func(context.Context) error) error {
			if loader == nil {
				return errors.New("delegate: the task recorder cannot load turns")
			}
			ctx, cancel := context.WithTimeout(ctx, watchLoadTimeout)
			defer cancel()
			return f(ctx)
		}
		var turns []dashi.Turn
		err := load(func(ctx context.Context) (err error) {
			turns, err = loadTurns(ctx, loader, t.sessionID)
			return err
		})
		if ctx.Err() != nil {
			return
		}
		w.opened(turns, err)
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		defer timer.Stop()
		for {
			events, lost, closed := q.take()
			for _, e := range events {
				w.push(e)
			}
			for _, e := range lost {
				w.push(e)
			}
			for _, update := range w.take() {
				if !yield(update, nil) {
					return
				}
			}
			if closed {
				return
			}
			due, next := w.checkMissing(time.Now())
			for _, turnID := range due {
				var turn dashi.Turn
				err := load(func(ctx context.Context) (err error) {
					turn, err = loader.Load(ctx, turnID)
					return err
				})
				if ctx.Err() != nil {
					return
				}
				w.reloaded(turnID, turn, err)
			}
			if len(due) > 0 {
				// Events arrived during the loads.
				continue
			}
			if next > 0 {
				timer.Reset(next)
			}
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			case <-timer.C:
			}
			timer.Stop()
		}
	}
}

// loadTurns loads the turns of a child session from turn 1 until one is
// missing or empty.
func loadTurns(ctx context.Context, loader dashi.Loader, sessionID string) ([]dashi.Turn, error) {
	var turns []dashi.Turn
	for n := 1; ; n++ {
		turn, err := loader.Load(ctx, session.TurnID(sessionID, n))
		if errors.Is(err, dashi.ErrTurnNotFound) || err == nil && len(turn.Steps) == 0 {
			return turns, nil
		}
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
}

// watchQueue holds the live events of one watch until its goroutine takes
// them. The child's observer goroutine fills it and never blocks on it.
type watchQueue struct {
	wake chan struct{}

	mu       sync.Mutex
	events   []dashi.Event
	previews int
	// lost holds one gap for each message whose deltas the queue dropped.
	lost   map[string]dashi.Event
	closed bool
}

func (q *watchQueue) add(e dashi.Event) {
	q.mu.Lock()
	switch {
	case e.Step != nil:
		q.events = append(q.events, e)
	case q.previews < watchPreviewLimit:
		q.previews++
		q.events = append(q.events, e)
	case e.Delta != nil || e.Gap:
		key := previewKey(e)
		if _, ok := q.lost[key]; !ok {
			gap := e
			gap.Delta, gap.Gap = nil, true
			q.lost[key] = gap
		}
	}
	q.mu.Unlock()
	q.signal()
}

func (q *watchQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns the queued events, the gaps of messages that lost deltas,
// and whether Close ran.
func (q *watchQueue) take() (events, lost []dashi.Event, closed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	events, q.events, q.previews = q.events, nil, 0
	for _, gap := range q.lost {
		lost = append(lost, gap)
	}
	clear(q.lost)
	return events, lost, q.closed
}

// listen adds a queue for the live events of task id. It returns nil after
// Close.
func (ts *Tasks) listen(id string) *watchQueue {
	ts.watchMu.Lock()
	defer ts.watchMu.Unlock()
	if ts.watchClosed {
		return nil
	}
	q := &watchQueue{wake: make(chan struct{}, 1), lost: map[string]dashi.Event{}}
	if ts.watches == nil {
		ts.watches = map[string][]*watchQueue{}
	}
	ts.watches[id] = append(ts.watches[id], q)
	return q
}

func (ts *Tasks) unlisten(id string, q *watchQueue) {
	ts.watchMu.Lock()
	defer ts.watchMu.Unlock()
	ts.watches[id] = slices.DeleteFunc(ts.watches[id], func(o *watchQueue) bool { return o == q })
	if len(ts.watches[id]) == 0 {
		delete(ts.watches, id)
	}
}

// watched hands a live child event of task id to its watches.
func (ts *Tasks) watched(id string, e dashi.Event) {
	ts.watchMu.Lock()
	queues := ts.watches[id]
	ts.watchMu.Unlock()
	for _, q := range queues {
		q.add(e)
	}
}

// closeWatches ends every watch, now and later.
func (ts *Tasks) closeWatches() {
	ts.watchMu.Lock()
	defer ts.watchMu.Unlock()
	ts.watchClosed = true
	for _, queues := range ts.watches {
		for _, q := range queues {
			q.mu.Lock()
			q.closed = true
			q.mu.Unlock()
			q.signal()
		}
	}
}

// watchState orders the events of one watch. Only the watch's goroutine
// touches it.
type watchState struct {
	// loading is set until the record that starts the watch has loaded;
	// every event waits until then.
	loading bool
	// resync is set after the start load failed: a Turn the watch meets
	// later starts at its first step that arrives.
	resync bool
	turns  map[string]*watchTurn
	// held holds the events of each Turn that cannot be yielded yet, in
	// the order they arrived.
	held         map[string][]dashi.Event
	heldPreviews int
	out          []TaskUpdate
}

// watchTurn is the state of one child Turn in a watch.
type watchTurn struct {
	id      string
	lastSeq int64
	// missingSince is when the watch first saw a missing step, or zero.
	missingSince time.Time
	// reloading is set while the Turn loads from the record. reloadTo is
	// the last missing Seq when the load started: the record must reach it
	// to fill the gap.
	reloading bool
	reloadTo  int64
	// resync makes the next step that arrives the Turn's next step.
	resync bool
	// skip holds the messages whose deltas are dropped until their step
	// commits, after a gap; done holds the messages and tool calls whose
	// step has committed.
	skip map[string]bool
	done map[string]bool
}

func newWatchState() *watchState {
	return &watchState{loading: true, turns: map[string]*watchTurn{}, held: map[string][]dashi.Event{}}
}

// turn returns the state of Turn id, adding it if it is new.
func (w *watchState) turn(id string) *watchTurn {
	t := w.turns[id]
	if t == nil {
		t = &watchTurn{id: id, resync: w.resync, skip: map[string]bool{}, done: map[string]bool{}}
		w.turns[id] = t
	}
	return t
}

// take returns the updates ready to yield and forgets them.
func (w *watchState) take() []TaskUpdate {
	out := w.out
	w.out = nil
	return out
}

// opened applies the record that starts the watch, or its load error.
func (w *watchState) opened(turns []dashi.Turn, err error) {
	w.loading = false
	if err != nil {
		w.out = append(w.out, TaskUpdate{Kind: TaskUpdateMissingSteps, Err: err}, TaskUpdate{Kind: TaskUpdateRecordEnd})
		w.resync = true
		for _, t := range w.turns {
			t.resync = true
		}
		w.settleAll()
		return
	}
	for i := range turns {
		turn := turns[i]
		t := w.turn(turn.ID)
		t.resync = false
		for _, s := range turn.Steps {
			t.markDone(s)
		}
		if n := len(turn.Steps); n > 0 {
			t.lastSeq = turn.Steps[n-1].Seq
		}
		w.out = append(w.out, TaskUpdate{Kind: TaskUpdateTurn, Turn: &turn})
	}
	w.out = append(w.out, TaskUpdate{Kind: TaskUpdateRecordEnd})
	w.settleAll()
}

// push takes one live event.
func (w *watchState) push(e dashi.Event) {
	if e.TurnID == "" {
		return
	}
	t := w.turn(e.TurnID)
	if w.waits(t) {
		w.hold(e)
		return
	}
	if e.Step != nil && t.resync {
		t.lastSeq, t.resync = e.Seq-1, false
	}
	switch {
	case e.Step != nil && e.Seq <= t.lastSeq:
		// Already yielded.
	case e.Step != nil && e.Seq == t.lastSeq+1:
		w.step(t, e)
		w.settleAll()
	case e.Seq > t.lastSeq || e.Step != nil:
		w.hold(e)
	default:
		w.preview(t, e)
	}
}

// waits reports whether Turn t's events must wait: for the start load, for
// t's reload, or for an earlier Turn that misses a step.
func (w *watchState) waits(t *watchTurn) bool {
	if w.loading || t.reloading {
		return true
	}
	n := turnNumber(t.id)
	for id, held := range w.held {
		if id != t.id && turnNumber(id) < n && w.missesStep(w.turn(id), held) {
			return true
		}
	}
	return false
}

// missesStep reports whether held holds a step of t that cannot follow yet.
func (w *watchState) missesStep(t *watchTurn, held []dashi.Event) bool {
	return slices.ContainsFunc(held, func(e dashi.Event) bool { return e.Step != nil && e.Seq > t.lastSeq })
}

// hold keeps an event until it can be yielded. Past watchPreviewLimit held
// previews, every held delta becomes one gap of its message.
func (w *watchState) hold(e dashi.Event) {
	if e.Step == nil && !e.Gap {
		w.heldPreviews++
	}
	w.held[e.TurnID] = append(w.held[e.TurnID], e)
	if w.heldPreviews > watchPreviewLimit {
		w.dropPreviews()
	}
}

func (w *watchState) dropPreviews() {
	for id, held := range w.held {
		gaps := map[string]bool{}
		out := held[:0]
		for _, e := range held {
			switch {
			case e.Step != nil:
				out = append(out, e)
			case e.Gap || e.Delta != nil:
				key := previewKey(e)
				if !gaps[key] {
					gaps[key] = true
					e.Delta, e.Gap = nil, true
					out = append(out, e)
				}
			}
		}
		clear(held[len(out):])
		w.held[id] = out
	}
	w.heldPreviews = 0
}

// settleAll yields the held events that can follow now, Turn by Turn in
// Turn order.
func (w *watchState) settleAll() {
	ids := make([]string, 0, len(w.held))
	for id := range w.held {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return turnNumber(a) - turnNumber(b) })
	for _, id := range ids {
		w.settle(w.turn(id))
	}
}

// settle yields the held events of Turn t that now follow its last step.
func (w *watchState) settle(t *watchTurn) {
	if w.waits(t) {
		return
	}
	held := w.held[t.id]
	if t.resync {
		if i := firstStep(held); i >= 0 {
			t.lastSeq, t.resync = held[i].Seq-1, false
		}
	}
	for {
		// Previews that belong after a yielded step, in arrival order, and
		// steps already yielded.
		held = slices.DeleteFunc(held, func(e dashi.Event) bool {
			if e.Seq > t.lastSeq {
				return false
			}
			if e.Step == nil {
				if !e.Gap {
					w.heldPreviews--
				}
				w.preview(t, e)
			}
			return true
		})
		i := slices.IndexFunc(held, func(e dashi.Event) bool { return e.Step != nil && e.Seq == t.lastSeq+1 })
		if i < 0 {
			break
		}
		e := held[i]
		held = slices.Delete(held, i, i+1)
		w.step(t, e)
	}
	if len(held) == 0 {
		delete(w.held, t.id)
	} else {
		w.held[t.id] = held
	}
	if firstStep(held) < 0 {
		t.missingSince = time.Time{}
	}
}

// firstStep returns the index of the held step with the lowest Seq, or -1.
func firstStep(held []dashi.Event) int {
	best := -1
	for i, e := range held {
		if e.Step != nil && (best < 0 || e.Seq < held[best].Seq) {
			best = i
		}
	}
	return best
}

// step yields the step that follows Turn t's last one.
func (w *watchState) step(t *watchTurn, e dashi.Event) {
	t.lastSeq = e.Seq
	t.markDone(*e.Step)
	// A later held step is a new gap, so its wait starts when
	// checkMissing first sees it.
	t.missingSince = time.Time{}
	w.out = append(w.out, TaskUpdate{Kind: TaskUpdateEvent, Event: e})
}

// preview yields a delta, tool progress, or gap that belongs after Turn
// t's last step, unless it is stale.
func (w *watchState) preview(t *watchTurn, e dashi.Event) {
	if e.Seq < t.lastSeq {
		return
	}
	key := previewKey(e)
	if t.done[key] {
		return
	}
	if e.Gap {
		if t.skip[key] {
			return
		}
		t.skip[key] = true
	} else if e.Delta != nil && t.skip[key] {
		return
	}
	w.out = append(w.out, TaskUpdate{Kind: TaskUpdateEvent, Event: e})
}

// checkMissing returns the Turns whose missing step has not arrived
// watchMissingWait after the watch first saw the gap, and marks them as
// reloading. It also returns how long until the next gap that still waits
// is due, or zero when none waits.
func (w *watchState) checkMissing(now time.Time) (due []string, next time.Duration) {
	if w.loading {
		return nil, 0
	}
	ids := make([]string, 0, len(w.held))
	for id := range w.held {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return turnNumber(a) - turnNumber(b) })
	for _, id := range ids {
		t := w.turn(id)
		i := firstStep(w.held[id])
		if t.reloading || i < 0 || w.waits(t) {
			continue
		}
		if t.missingSince.IsZero() {
			t.missingSince = now
		}
		wait := watchMissingWait - now.Sub(t.missingSince)
		if wait <= 0 {
			t.reloading, t.reloadTo = true, w.held[id][i].Seq-1
			due = append(due, id)
			continue
		}
		if next == 0 || wait < next {
			next = wait
		}
	}
	return due, next
}

// reloaded applies the record of Turn id that a gap asked for, or its load
// error.
func (w *watchState) reloaded(id string, turn dashi.Turn, err error) {
	t := w.turn(id)
	reloadTo := t.reloadTo
	t.reloading, t.missingSince = false, time.Time{}
	if err != nil {
		w.missing(t, err)
		return
	}
	for i := range turn.Steps {
		s := turn.Steps[i]
		if s.Seq == t.lastSeq+1 {
			w.step(t, dashi.Event{TurnID: id, Seq: s.Seq, Step: &s})
		}
	}
	w.settleAll()
	// Only a record that ends before the missing step proves it lost. A
	// step still held past a record that covers the gap is a new gap, from
	// events that arrived during the load; it waits again.
	if firstStep(w.held[id]) >= 0 && t.lastSeq < reloadTo {
		w.missing(t, fmt.Errorf("delegate: the record of turn %s lacks step %d", id, t.lastSeq+1))
	}
}

// missing reports that Turn t lost steps and continues from its first held
// step.
func (w *watchState) missing(t *watchTurn, err error) {
	w.out = append(w.out, TaskUpdate{Kind: TaskUpdateMissingSteps, TurnID: t.id, Err: err})
	t.resync = true
	w.settleAll()
}

// markDone records that a step committed a message or a tool call, so
// later previews of it are stale.
func (t *watchTurn) markDone(s dashi.Step) {
	switch {
	case s.Model != nil && (s.Kind == dashi.StepModelCallFinished || s.Kind == dashi.StepModelCallFinishedByHost):
		key := "m:" + s.Model.MessageID + "/" + s.Model.ModelCallID
		t.done[key] = true
		delete(t.skip, key)
	case s.Kind == dashi.StepToolFinished && s.Tool != nil:
		t.done["t:"+s.Tool.Call.ID] = true
	}
}

// previewKey names the message or tool call a preview belongs to.
func previewKey(e dashi.Event) string {
	if e.Progress != nil {
		return "t:" + e.Progress.CallID
	}
	return "m:" + e.MessageID + "/" + e.ModelCallID
}

// turnNumber is the number of a session turn ID (session.TurnID), or 0.
func turnNumber(id string) int {
	n, _ := strconv.Atoi(id[strings.LastIndexByte(id, '-')+1:])
	return n
}
