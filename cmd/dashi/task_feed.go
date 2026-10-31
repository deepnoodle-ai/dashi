package main

import (
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

// A task feed renders one task's child events into its task pane, on the
// loop. It shows each step once and in order: it first replays the recorded
// child Turns, then applies live events after them. Live events that arrive
// before the record, or after a missing step, wait in the feed until they
// can apply.

// taskPreviewCap bounds the previews (deltas and tool progress) one feed
// holds unapplied. Past it, the feed drops them and marks their messages as
// having a gap. Steps are never dropped.
const taskPreviewCap = 4096

// taskFactsID names the facts block at the top of a task pane.
const taskFactsID = "task-facts"

// taskLoadKind is why a feed needs the task's record.
type taskLoadKind int

const (
	// taskLoadOpen fills a pane that just opened.
	taskLoadOpen taskLoadKind = iota
	// taskLoadReload replaces a Turn that has a missing step.
	taskLoadReload
	// taskLoadFacts refreshes only the facts block.
	taskLoadFacts
)

// taskLoad is a request for the task's record, and what came back.
type taskLoad struct {
	kind taskLoadKind
	// turn is the child Turn a reload replaces.
	turn string
	// pane and token name the pane and the open that asked; a result for
	// a pane that closed, or that opened again since, is dropped.
	pane     string
	token    uint64
	snapshot taskSnapshot
	turns    []dashi.Turn
	err      error
}

// taskFeed is the live state of one task pane. Only the loop touches it.
type taskFeed struct {
	task  string
	token uint64
	dir   string
	view  *transcriptPane
	// replaying is set until the open load has been applied; every event
	// waits until then.
	replaying bool
	// resync is set after a failed open load: a Turn the feed meets later
	// starts at its first step that arrives.
	resync bool
	turns  map[string]*feedTurn
	// held holds the events of each Turn that cannot apply yet, in the
	// order they arrived.
	held         map[string][]dashi.Event
	heldPreviews int
	// loads are the record loads the screen should start.
	loads []taskLoad
	// menuStale reports that a block the transcript menu acts on changed.
	menuStale bool
}

// feedTurn is the state of one child Turn in a feed.
type feedTurn struct {
	id string
	r  *Renderer
	// steps are the Turn's steps so far, from which the footer is built.
	steps   []dashi.Step
	lastSeq int64
	// start is the index of the Turn's first block in the pane, or -1.
	start int
	// gapSince is when the feed first saw a missing step, or zero.
	gapSince  time.Time
	reloading bool
	// reloadTo is the last missing Seq when the reload was asked for: the
	// record must reach it to fill the gap.
	reloadTo int64
	// resync makes the next step that arrives the Turn's next step.
	resync bool
	// skip holds the messages whose previews are ignored until their
	// step commits, after a gap; done holds the messages and tool calls
	// whose step has committed.
	skip map[string]bool
	done map[string]bool
}

func newTaskFeed(task string, token uint64, dir string, view *transcriptPane) *taskFeed {
	return &taskFeed{task: task, token: token, dir: dir, view: view, replaying: true, turns: map[string]*feedTurn{}, held: map[string][]dashi.Event{}}
}

// turn returns the state of Turn id, adding it if it is new.
func (f *taskFeed) turn(id string) *feedTurn {
	t := f.turns[id]
	if t == nil {
		t = &feedTurn{id: id, start: -1, resync: f.resync, skip: map[string]bool{}, done: map[string]bool{}}
		f.turns[id] = t
	}
	return t
}

// push takes one live event of the task.
func (f *taskFeed) push(ev dashi.Event) {
	if ev.TurnID == "" {
		return
	}
	t := f.turn(ev.TurnID)
	if f.replaying || t.reloading {
		f.hold(ev)
		return
	}
	if ev.Step != nil && t.resync {
		t.lastSeq, t.resync = ev.Seq-1, false
	}
	switch {
	case ev.Step != nil && ev.Seq <= t.lastSeq:
		// Already shown.
	case ev.Step != nil && ev.Seq == t.lastSeq+1:
		f.step(t, ev)
		f.settle(t)
	case ev.Seq > t.lastSeq || ev.Step != nil:
		f.hold(ev)
	default:
		f.preview(t, ev)
	}
}

// hold keeps an event until it can apply.
func (f *taskFeed) hold(ev dashi.Event) {
	f.held[ev.TurnID] = append(f.held[ev.TurnID], ev)
	if ev.Step != nil {
		return
	}
	f.heldPreviews++
	if f.heldPreviews > taskPreviewCap {
		f.dropPreviews()
	}
}

// dropPreviews drops every held preview and gives their messages a gap.
func (f *taskFeed) dropPreviews() {
	for id, held := range f.held {
		t := f.turn(id)
		f.held[id] = slices.DeleteFunc(held, func(ev dashi.Event) bool {
			if ev.Step != nil {
				return false
			}
			if ev.Delta != nil {
				f.gap(t, previewKey(ev))
			}
			return true
		})
	}
	f.heldPreviews = 0
}

// settle applies the held events of Turn t that now follow its last step.
func (f *taskFeed) settle(t *feedTurn) {
	if f.replaying || t.reloading {
		return
	}
	held := f.held[t.id]
	if t.resync {
		if i := firstStep(held); i >= 0 {
			t.lastSeq, t.resync = held[i].Seq-1, false
		}
	}
	for {
		// Previews that belong after a shown step, in arrival order, and
		// steps already shown.
		held = slices.DeleteFunc(held, func(ev dashi.Event) bool {
			if ev.Seq > t.lastSeq {
				return false
			}
			if ev.Step == nil {
				f.heldPreviews--
				f.preview(t, ev)
			}
			return true
		})
		i := slices.IndexFunc(held, func(ev dashi.Event) bool { return ev.Step != nil && ev.Seq == t.lastSeq+1 })
		if i < 0 {
			break
		}
		ev := held[i]
		held = slices.Delete(held, i, i+1)
		f.step(t, ev)
	}
	if len(held) == 0 {
		delete(f.held, t.id)
	} else {
		f.held[t.id] = held
	}
	if firstStep(held) < 0 {
		t.gapSince = time.Time{}
	}
}

// settleAll settles every Turn that holds events.
func (f *taskFeed) settleAll() {
	for _, id := range f.heldTurns() {
		f.settle(f.turn(id))
	}
}

// heldTurns returns the IDs of the Turns that hold events, in Turn order.
func (f *taskFeed) heldTurns() []string {
	ids := make([]string, 0, len(f.held))
	for id := range f.held {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return turnNumber(a) - turnNumber(b) })
	return ids
}

// firstStep returns the index of the held step with the lowest Seq, or -1.
func firstStep(held []dashi.Event) int {
	best := -1
	for i, ev := range held {
		if ev.Step != nil && (best < 0 || ev.Seq < held[best].Seq) {
			best = i
		}
	}
	return best
}

// renderer returns Turn t's renderer, starting one from its number.
func (f *taskFeed) renderer(t *feedTurn) *Renderer {
	if t.r == nil {
		t.r = NewRenderer(turnNumber(t.id), "", f.dir)
	}
	return t.r
}

// add shows one block of Turn t.
func (f *taskFeed) add(t *feedTurn, b Block) {
	if t.start < 0 {
		t.start = len(f.view.tr.Blocks)
	}
	if f.view.apply(b) {
		f.menuStale = true
	}
}

// step applies the step that follows Turn t's last one. A step that stops
// the Turn closes it with its footer, as the main pane does, and refreshes
// the facts block.
func (f *taskFeed) step(t *feedTurn, ev dashi.Event) {
	s := ev.Step
	t.lastSeq = ev.Seq
	t.steps = append(t.steps, *s)
	t.markDone(*s)
	for _, b := range f.renderer(t).Event(ev) {
		f.add(t, b)
	}
	if s.Kind == dashi.StepTurnStopped {
		if view, err := (dashi.Turn{ID: t.id, Steps: t.steps}).View(); err == nil {
			f.close(t, dashi.Turn{ID: t.id, Steps: t.steps}, view)
		}
		f.loads = append(f.loads, taskLoad{kind: taskLoadFacts})
	}
}

// close adds what follows a stopped Turn: the active-context outcome and
// the footer, as Engine.finished does for the main pane.
func (f *taskFeed) close(t *feedTurn, turn dashi.Turn, view dashi.TurnView) {
	r := f.renderer(t)
	if view.Origin == dashi.StartCompaction {
		f.add(t, r.compactionOutcome(view))
	}
	for _, b := range r.activeContextStopped(view.Stop) {
		f.add(t, b)
	}
	r.Model = turnModel(turn, r.Model)
	f.add(t, f.footer(r, view, turn.Steps))
}

// footer is a Turn's footer, labeled with the task.
func (f *taskFeed) footer(r *Renderer, view dashi.TurnView, steps []dashi.Step) Block {
	b := r.Footer(view, steps)
	b.Footer.Task = f.task
	b.Text = b.Footer.String()
	return b
}

// markDone records that a step committed a message or a tool call, so
// later previews of it are stale.
func (t *feedTurn) markDone(s dashi.Step) {
	switch {
	case s.Model != nil && (s.Kind == dashi.StepModelCallFinished || s.Kind == dashi.StepModelCallFinishedByHost):
		key := messageKey(s.Model.MessageID, s.Model.ModelCallID)
		t.done[key] = true
		delete(t.skip, key)
	case s.Kind == dashi.StepToolFinished && s.Tool != nil:
		t.done["t:"+s.Tool.Call.ID] = true
	}
}

// previewKey names the block a preview updates.
func previewKey(ev dashi.Event) string {
	if ev.Progress != nil {
		return "t:" + ev.Progress.CallID
	}
	return messageKey(ev.MessageID, ev.ModelCallID)
}

// preview applies a delta, tool progress, or gap that belongs after Turn
// t's last step. One placed before that step, or whose message or call has
// committed, is stale.
func (f *taskFeed) preview(t *feedTurn, ev dashi.Event) {
	if ev.Seq < t.lastSeq {
		return
	}
	key := previewKey(ev)
	if ev.Gap {
		f.gap(t, key)
		return
	}
	if t.done[key] || ev.Delta != nil && t.skip[key] {
		return
	}
	for _, b := range f.renderer(t).Event(ev) {
		f.add(t, b)
	}
}

// gap drops the preview text of a message and ignores its deltas until its
// step commits.
func (f *taskFeed) gap(t *feedTurn, key string) {
	if t.done[key] {
		return
	}
	t.skip[key] = true
	r := f.renderer(t)
	if r.text[key] != "" {
		delete(r.text, key)
		f.add(t, Block{ID: key, Kind: KindAssistant})
	}
}

// checkGaps starts a reload of each Turn whose missing step has not
// arrived taskGapWait after the feed first saw the gap. It returns how long
// until the next gap that still waits is due, or zero when none waits, so
// the screen can flush again then.
func (f *taskFeed) checkGaps(now time.Time) (next time.Duration) {
	if f.replaying {
		return 0
	}
	for _, id := range f.heldTurns() {
		t := f.turn(id)
		i := firstStep(f.held[id])
		if t.reloading || i < 0 {
			continue
		}
		if t.gapSince.IsZero() {
			t.gapSince = now
		}
		wait := taskGapWait - now.Sub(t.gapSince)
		if wait <= 0 {
			t.reloading, t.reloadTo = true, f.held[id][i].Seq-1
			f.loads = append(f.loads, taskLoad{kind: taskLoadReload, turn: id})
			continue
		}
		if next == 0 || wait < next {
			next = wait
		}
	}
	return next
}

// takeLoads returns the loads to start and forgets them.
func (f *taskFeed) takeLoads() []taskLoad {
	loads := f.loads
	f.loads = nil
	return loads
}

// loaded applies a load result whose token matches the pane's open.
func (f *taskFeed) loaded(l taskLoad) {
	switch l.kind {
	case taskLoadOpen:
		f.replaying = false
		if l.err != nil {
			f.view.apply(Block{ID: taskFactsID, Kind: KindWarning, Text: "Task record unavailable: " + l.err.Error() + ". Earlier steps are not shown; the pane continues from the next live step.", Done: true})
			f.resync = true
			for _, t := range f.turns {
				t.resync = true
			}
			f.settleAll()
			return
		}
		f.facts(l.snapshot)
		f.replay(l.turns, 0)
		f.settleAll()
	case taskLoadReload:
		t := f.turn(l.turn)
		t.reloading, t.gapSince = false, time.Time{}
		reloadTo := t.reloadTo
		if l.err != nil {
			f.missing(t, l.err.Error())
			return
		}
		cut := t.start
		if cut < 0 {
			cut = len(f.view.tr.Blocks)
		}
		f.view.truncate(cut)
		f.menuStale = true
		from := turnNumber(l.turn)
		for id, other := range f.turns {
			if other.start >= cut || id == l.turn {
				delete(f.turns, id)
			}
		}
		f.replay(l.turns, from)
		f.turn(l.turn).reloading = false
		f.settleAll()
		// Only a record that ends before the missing step proves it lost.
		// A step still held past a record that covers the gap is a new
		// gap, from events that arrived during the load; it waits again.
		if t := f.turn(l.turn); firstStep(f.held[l.turn]) >= 0 && t.lastSeq < reloadTo {
			f.missing(t, "the record lacks it too")
		}
	case taskLoadFacts:
		if l.err == nil {
			f.facts(l.snapshot)
		}
	}
}

// missing shows that Turn t lost steps and continues from its first held
// step, without another load.
func (f *taskFeed) missing(t *feedTurn, reason string) {
	f.add(t, warnBlock("Turn %d is missing steps (%s). The pane continues from the next step it has.", turnNumber(t.id), reason))
	t.resync = true
	f.settle(t)
}

// facts shows the task's description, one quiet line at the top of the
// pane. The title row carries the state and counts, and the details pane
// (/tasks ID) the full facts.
func (f *taskFeed) facts(snapshot taskSnapshot) {
	f.view.apply(hintBlock(taskFactsID, taskPaneHeader(snapshot.Task)))
}

// replay shows the recorded Turns numbered from and after, each through a
// fresh renderer the feed keeps, so live events continue its call state.
func (f *taskFeed) replay(turns []dashi.Turn, from int) {
	for _, turn := range turns {
		n := turnNumber(turn.ID)
		if n < from {
			continue
		}
		// A gap found while the record loaded still holds.
		var skip map[string]bool
		if old := f.turns[turn.ID]; old != nil {
			skip = old.skip
		}
		delete(f.turns, turn.ID)
		t := f.turn(turn.ID)
		t.resync = false
		if skip != nil {
			t.skip = skip
		}
		view, err := turn.View()
		if err != nil {
			f.add(t, warnBlock("Turn %d skipped: %v.", n, err))
			t.resync = true
			continue
		}
		r := f.renderer(t)
		compaction := view.Origin == dashi.StartCompaction
		for i := range turn.Steps {
			step := turn.Steps[i]
			blocks := r.Event(dashi.Event{TurnID: turn.ID, Seq: step.Seq, Step: &step})
			t.markDone(step)
			if compaction {
				continue
			}
			for _, b := range blocks {
				f.add(t, b)
			}
		}
		t.steps = slices.Clone(turn.Steps)
		// A running Turn has no stop step yet; its view would infer one.
		stopped := false
		if n := len(turn.Steps); n > 0 {
			t.lastSeq = turn.Steps[n-1].Seq
			stopped = turn.Steps[n-1].Kind == dashi.StepTurnStopped
		}
		switch {
		case compaction:
			// A reopened compaction shows its current state, as RenderTurn
			// does.
			stop := dashi.Stop{}
			if stopped {
				stop = view.Stop
			}
			r.summary = view.Reply
			f.add(t, r.compactionBlock(view.ID, stop))
			if stopped {
				f.add(t, f.footer(r, view, turn.Steps))
			}
		case stopped:
			f.close(t, turn, view)
		}
	}
}
