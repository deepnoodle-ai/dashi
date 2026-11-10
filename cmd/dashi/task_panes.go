package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
)

// taskSource is where task panes watch tasks and load their facts: the
// Engine, or a fake in tests.
type taskSource interface {
	WatchTask(ctx context.Context, id string) iter.Seq2[delegate.TaskUpdate, error]
	TaskFacts(ctx context.Context, id string) (taskSnapshot, error)
}

// taskWatchUpdate is one update of the task pane that pane and token name.
type taskWatchUpdate struct {
	pane   string
	token  uint64
	update delegate.TaskUpdate
	err    error
}

// taskFactsLoad is a load of a task pane's facts, and what came back. open
// is set for the load that fills a pane that just opened.
type taskFactsLoad struct {
	open bool
	// pane and token name the pane and the open that asked; a result for
	// a pane that closed, or that opened again since, is dropped.
	pane     string
	token    uint64
	snapshot taskSnapshot
	err      error
}

// taskFactsID names the facts block at the top of a task pane, and
// taskRecordID the warning that the task's record did not load.
const (
	taskFactsID  = "task-facts"
	taskRecordID = "task-record"
)

// taskLoadTimeout is how long a task facts load may take.
const taskLoadTimeout = 30 * time.Second

// taskWatchTip is the one-time hint the first task line of a session
// shows, with that task's ID.
func taskWatchTip(id string) string {
	return "Click a task, or type /task " + id + ", to watch it here."
}

// taskPaneID is the pane ID of task id.
func taskPaneID(id string) string { return "task:" + id }

// OpenTask opens or focuses the pane of task id, as a bare /task ID does in
// fullscreen mode. It implements panesUI.
func (s *screen) OpenTask(id string) {
	s.TranscriptCommand("/task", id)
}

// openTask opens the pane of task id and activates it, or activates it if
// it is open and refreshes its facts. A new pane runs delegate.Tasks.Watch
// off the loop and queues each update with the pane's open token.
func (s *screen) openTask(id string) {
	if p := s.panes.get(taskPaneID(id)); p != nil {
		s.openPane(p)
		s.loadFacts(p, false)
		return
	}
	if s.tasks == nil {
		s.Emit(warnBlock("Task panes are unavailable."))
		return
	}
	p := s.newPane(taskPaneID(id), paneTask, id)
	s.taskOpens++
	p.watch = newTaskWatch(id, s.taskOpens, s.dir, p.view)
	p.state = "loading"
	p.view.apply(hintBlock(taskFactsID, "Loading the task record…"))
	if !s.openPane(p) {
		return
	}
	s.redrawTaskLines(id)
	src, pane, token := s.tasks, p.id, p.watch.token
	ctx, cancel := context.WithCancel(s.taskContext())
	go func() {
		for update, err := range src.WatchTask(ctx, id) {
			s.enqueue(queued{update: &taskWatchUpdate{pane: pane, token: token, update: update, err: err}})
			if err != nil {
				return
			}
		}
	}()
	p.onClose = func() {
		cancel()
		s.dropTaskQueue(pane, token)
	}
	s.loadFacts(p, true)
	s.refreshWaiting()
}

// taskContext is the context task panes watch and load under.
func (s *screen) taskContext() context.Context {
	if s.ctrl != nil {
		return s.ctrl.base
	}
	return context.Background()
}

// loadFacts loads the facts of p's task off the loop, within
// taskLoadTimeout, and queues the result with the pane's open token.
func (s *screen) loadFacts(p *pane, open bool) {
	if s.tasks == nil || p.watch == nil {
		return
	}
	src, id := s.tasks, p.label
	l := taskFactsLoad{open: open, pane: p.id, token: p.watch.token}
	var broker *approvalBroker
	if s.ctrl != nil {
		broker = s.ctrl.broker
	}
	timeout := taskLoadTimeout
	if s.taskTimeout > 0 {
		timeout = s.taskTimeout
	}
	base := s.taskContext()
	go func() {
		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		l.snapshot, l.err = src.TaskFacts(ctx, id)
		if l.err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			l.err = fmt.Errorf("the load took longer than %v", timeout)
		}
		if broker != nil && l.err == nil {
			if owner, active := broker.activeOwner(); active && owner.TaskID == l.snapshot.Task.SessionID {
				l.snapshot.Prompt = &owner.Call
			}
		}
		s.enqueue(queued{load: &l})
	}()
}

// dropTaskQueue drops the queued updates and loads of a pane that closed.
func (s *screen) dropTaskQueue(pane string, token uint64) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	s.pending = slices.DeleteFunc(s.pending, func(q queued) bool {
		return q.update != nil && q.update.pane == pane && q.update.token == token ||
			q.load != nil && q.load.pane == pane && q.load.token == token
	})
}

// taskPaneOf returns the open pane that pane and token name, or nil when
// that pane closed or opened again since.
func (s *screen) taskPaneOf(pane string, token uint64) *pane {
	p := s.panes.get(pane)
	if p == nil || p.watch == nil || p.watch.token != token {
		return nil
	}
	return p
}

// taskUpdate applies one update to its task pane.
func (s *screen) taskUpdate(u taskWatchUpdate) {
	p := s.taskPaneOf(u.pane, u.token)
	if p == nil {
		return
	}
	p.watch.apply(u.update, u.err)
	if p.watch.factsDue {
		p.watch.factsDue = false
		s.loadFacts(p, false)
	}
	s.taskMenuCheck(p)
}

// taskFactsLoaded applies a task facts load to its task pane.
func (s *screen) taskFactsLoaded(l taskFactsLoad) {
	p := s.taskPaneOf(l.pane, l.token)
	if p == nil {
		return
	}
	if l.err != nil {
		if l.open {
			p.state, p.title = "record unavailable", ""
			p.view.apply(Block{ID: taskFactsID, Kind: KindWarning, Text: "Task facts unavailable: " + l.err.Error() + ".", Done: true})
		}
		return
	}
	v := l.snapshot.Task
	if l.open {
		p.subagent = v.Subagent
		p.state, p.title = taskTitle(v)
		p.ended = !v.Running() && v.State != taskApprovalPending
	}
	// One quiet line at the top of the pane. The title row carries the
	// state and counts, and the details pane (/tasks ID) the full facts.
	p.view.apply(hintBlock(taskFactsID, taskPaneHeader(v)))
}

// taskMenuCheck closes the transcript menu when the item it acts on in p
// changed.
func (s *screen) taskMenuCheck(p *pane) {
	if p.watch.menuStale {
		p.watch.menuStale = false
		if s.transcriptMenuPane == p.id {
			s.transcriptMenu.Close()
		}
	}
}

// taskLine updates the title of a task pane from the task's status line,
// which is emitted on every child step and task event.
func (s *screen) taskLine(b Block) {
	if b.Kind != KindTaskStatus {
		return
	}
	p := s.panes.get(taskPaneID(taskIDOf(b.Arg)))
	if p == nil || p.watch == nil {
		return
	}
	p.state, p.title = taskLineTitle(b)
	p.ended = b.Done
}

// A taskWatch renders the updates of one task's watch into its task pane,
// on the loop. delegate.Tasks.Watch orders them; the pane shows each as it
// comes.
type taskWatch struct {
	task  string
	token uint64
	dir   string
	view  *transcriptPane
	// loading is set until the watch has yielded the task's record.
	loading bool
	turns   map[string]*taskWatchTurn
	// factsDue asks the screen for fresh facts, after a Turn stopped.
	factsDue bool
	// menuStale reports that a block the transcript menu acts on changed.
	menuStale bool
}

// taskWatchTurn is the state of one child Turn in a task pane.
type taskWatchTurn struct {
	id string
	r  *Renderer
	// steps are the Turn's steps so far, from which the footer is built.
	steps []dashi.Step
}

func newTaskWatch(task string, token uint64, dir string, view *transcriptPane) *taskWatch {
	return &taskWatch{task: task, token: token, dir: dir, view: view, loading: true, turns: map[string]*taskWatchTurn{}}
}

// turn returns the state of Turn id, adding it if it is new.
func (w *taskWatch) turn(id string) *taskWatchTurn {
	t := w.turns[id]
	if t == nil {
		t = &taskWatchTurn{id: id}
		w.turns[id] = t
	}
	return t
}

// apply shows one update of the watch, or the error that ended it.
func (w *taskWatch) apply(u delegate.TaskUpdate, err error) {
	if err != nil {
		w.loading = false
		w.view.apply(Block{ID: taskRecordID, Kind: KindWarning, Text: "Task watch ended: " + err.Error() + ".", Done: true})
		return
	}
	switch u.Kind {
	case delegate.TaskUpdateTurn:
		w.replay(*u.Turn)
	case delegate.TaskUpdateLive:
		w.loading = false
	case delegate.TaskUpdateMissingSteps:
		if u.TurnID == "" {
			w.view.apply(Block{ID: taskRecordID, Kind: KindWarning, Text: "Task record unavailable: " + u.Err.Error() + ". Earlier steps are not shown; the pane continues from the next live step.", Done: true})
			return
		}
		w.add(w.turn(u.TurnID), warnBlock("Turn %d is missing steps (%v). The pane continues from the next step it has.", turnNumber(u.TurnID), u.Err))
	case delegate.TaskUpdateEvent:
		t := w.turn(u.Event.TurnID)
		if u.Event.Step != nil {
			w.step(t, u.Event)
		} else {
			w.preview(t, u.Event)
		}
	}
}

// renderer returns Turn t's renderer, starting one from its number.
func (w *taskWatch) renderer(t *taskWatchTurn) *Renderer {
	if t.r == nil {
		t.r = NewRenderer(turnNumber(t.id), "", w.dir)
	}
	return t.r
}

// add shows one block of Turn t.
func (w *taskWatch) add(_ *taskWatchTurn, b Block) {
	if w.view.apply(b) {
		w.menuStale = true
	}
}

// step shows the next step of Turn t. A step that stops the Turn closes it
// with its footer, as the main pane does, and asks for fresh facts.
func (w *taskWatch) step(t *taskWatchTurn, ev dashi.Event) {
	s := ev.Step
	t.steps = append(t.steps, *s)
	for _, b := range w.renderer(t).Event(ev) {
		w.add(t, b)
	}
	if s.Kind == dashi.StepTurnStopped {
		turn := dashi.Turn{ID: t.id, Steps: t.steps}
		if view, err := turn.View(); err == nil {
			w.close(t, turn, view)
		}
		w.factsDue = true
	}
}

// close adds what follows a stopped Turn: the active-context outcome and
// the footer, as Engine.finished does for the main pane.
func (w *taskWatch) close(t *taskWatchTurn, turn dashi.Turn, view dashi.TurnView) {
	r := w.renderer(t)
	if view.Origin == dashi.StartCompaction {
		w.add(t, r.compactionOutcome(view))
	}
	for _, b := range r.activeContextStopped(view.Stop) {
		w.add(t, b)
	}
	r.Model = turnModel(turn, r.Model)
	w.add(t, w.footer(r, view, turn.Steps))
}

// footer is a Turn's footer, labeled with the task.
func (w *taskWatch) footer(r *Renderer, view dashi.TurnView, steps []dashi.Step) Block {
	b := r.Footer(view, steps)
	b.Footer.Task = w.task
	b.Text = b.Footer.String()
	return b
}

// preview shows a delta or tool progress, or drops the preview text of a
// message whose deltas have a gap.
func (w *taskWatch) preview(t *taskWatchTurn, ev dashi.Event) {
	if ev.Gap {
		key := messageKey(ev.MessageID, ev.ModelCallID)
		r := w.renderer(t)
		if r.text[key] != "" {
			delete(r.text, key)
			w.add(t, Block{ID: key, Kind: KindAssistant})
		}
		return
	}
	for _, b := range w.renderer(t).Event(ev) {
		w.add(t, b)
	}
}

// replay shows a recorded Turn through a fresh renderer the pane keeps, so
// live events continue its call state.
func (w *taskWatch) replay(turn dashi.Turn) {
	delete(w.turns, turn.ID)
	t := w.turn(turn.ID)
	n := turnNumber(turn.ID)
	view, err := turn.View()
	if err != nil {
		w.add(t, warnBlock("Turn %d skipped: %v.", n, err))
		return
	}
	r := w.renderer(t)
	compaction := view.Origin == dashi.StartCompaction
	for i := range turn.Steps {
		step := turn.Steps[i]
		blocks := r.Event(dashi.Event{TurnID: turn.ID, Seq: step.Seq, Step: &step})
		if compaction {
			continue
		}
		for _, b := range blocks {
			w.add(t, b)
		}
	}
	t.steps = slices.Clone(turn.Steps)
	// A running Turn has no stop step yet; its view would infer one.
	stopped := false
	if n := len(turn.Steps); n > 0 {
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
		w.add(t, r.compactionBlock(view.ID, stop))
		if stopped {
			w.add(t, w.footer(r, view, turn.Steps))
		}
	case stopped:
		w.close(t, turn, view)
	}
}

// refreshWaiting marks the task panes whose task waits for the person, from
// the approval broker's active and queued prompts.
func (s *screen) refreshWaiting() {
	if s.panes.count() < 2 {
		return
	}
	var owners []brokerOwner
	if s.ctrl != nil && s.ctrl.broker != nil {
		owners = s.ctrl.broker.waitingOwners()
	}
	for _, id := range s.panes.leaves() {
		p := s.panes.get(id)
		if p == nil || p.kind != paneTask {
			continue
		}
		p.waiting = slices.ContainsFunc(owners, func(o brokerOwner) bool {
			return o.TaskID != "" && delegate.TaskID(o.TaskID) == p.label
		})
	}
}

// now is the time on the screen's clock.
func (s *screen) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
