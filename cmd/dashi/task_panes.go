package main

import (
	"context"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
)

// taskSource is where task panes watch tasks and load their records: the
// Engine, or a fake in tests.
type taskSource interface {
	WatchTask(id string, fn func(delegate.ChildEvent)) (unwatch func())
	TaskRecord(ctx context.Context, id string) (taskSnapshot, []dashi.Turn, error)
}

// taskChildEvent is a child event for the task pane that pane and token
// name.
type taskChildEvent struct {
	pane     string
	token    uint64
	subagent string
	event    dashi.Event
}

// taskGapEvent asks the loop for a flush, so a task feed's missing step
// gets its reload even when no other event comes.
type taskGapEvent struct{ at time.Time }

func (e taskGapEvent) Timestamp() time.Time { return e.at }

// taskGapWait is how long a task feed waits for a missing step before it
// reloads the Turn from the record.
const taskGapWait = 100 * time.Millisecond

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
// it is open and refreshes its facts. A new pane watches the task first,
// then loads its record off the loop; live events wait until the record
// has been shown.
func (s *screen) openTask(id string) {
	if p := s.panes.get(taskPaneID(id)); p != nil {
		s.openPane(p)
		s.loadTask(p, taskLoad{kind: taskLoadFacts})
		return
	}
	if s.tasks == nil {
		s.Emit(warnBlock("Task panes are unavailable."))
		return
	}
	p := s.newPane(taskPaneID(id), paneTask, id)
	s.taskOpens++
	p.feed = newTaskFeed(id, s.taskOpens, s.dir, p.view)
	p.state = "loading"
	p.view.apply(hintBlock(taskFactsID, "Loading the task record…"))
	if !s.openPane(p) {
		return
	}
	s.redrawTaskLines(id)
	pane, token := p.id, p.feed.token
	unwatch := s.tasks.WatchTask(id, func(ev delegate.ChildEvent) {
		s.enqueue(queued{child: &taskChildEvent{pane: pane, token: token, subagent: ev.Subagent, event: ev.Event}})
	})
	p.onClose = func() {
		unwatch()
		s.dropTaskQueue(pane, token)
	}
	s.loadTask(p, taskLoad{kind: taskLoadOpen})
	s.refreshWaiting()
}

// loadTask loads the record of p's task off the loop and queues the result
// with the pane's open token.
func (s *screen) loadTask(p *pane, l taskLoad) {
	if s.tasks == nil || p.feed == nil {
		return
	}
	src, id := s.tasks, p.label
	l.pane, l.token = p.id, p.feed.token
	ctx := context.Background()
	var broker *approvalBroker
	if s.ctrl != nil {
		ctx, broker = s.ctrl.base, s.ctrl.broker
	}
	go func() {
		l.snapshot, l.turns, l.err = src.TaskRecord(ctx, id)
		if broker != nil && l.err == nil {
			if owner, active := broker.activeOwner(); active && owner.TaskID == l.snapshot.Task.SessionID {
				l.snapshot.Prompt = &owner.Call
			}
		}
		s.enqueue(queued{load: &l})
	}()
}

// dropTaskQueue drops the queued events and loads of a pane that closed.
func (s *screen) dropTaskQueue(pane string, token uint64) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	s.pending = slices.DeleteFunc(s.pending, func(q queued) bool {
		return q.child != nil && q.child.pane == pane && q.child.token == token ||
			q.load != nil && q.load.pane == pane && q.load.token == token
	})
}

// taskFeedOf returns the open pane and feed that pane and token name, or
// nil when that pane closed or opened again since.
func (s *screen) taskFeedOf(pane string, token uint64) *pane {
	p := s.panes.get(pane)
	if p == nil || p.feed == nil || p.feed.token != token {
		return nil
	}
	return p
}

// taskChild applies one child event to its task pane.
func (s *screen) taskChild(e taskChildEvent) {
	p := s.taskFeedOf(e.pane, e.token)
	if p == nil {
		return
	}
	if p.subagent == "" {
		p.subagent = e.subagent
	}
	p.feed.push(e.event)
	s.taskMenuCheck(p)
}

// taskLoaded applies a task record load to its task pane.
func (s *screen) taskLoaded(l taskLoad) {
	p := s.taskFeedOf(l.pane, l.token)
	if p == nil {
		return
	}
	if l.kind == taskLoadOpen && l.err == nil {
		v := l.snapshot.Task
		p.subagent = v.Subagent
		p.state, p.title = taskTitle(v)
		p.ended = !v.Running() && v.State != taskApprovalPending
	}
	if l.kind == taskLoadOpen && l.err != nil {
		p.state, p.title = "record unavailable", ""
	}
	p.feed.loaded(l)
	s.taskMenuCheck(p)
}

// taskMenuCheck closes the transcript menu when the item it acts on in p
// changed.
func (s *screen) taskMenuCheck(p *pane) {
	if p.feed.menuStale {
		p.feed.menuStale = false
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
	if p == nil || p.feed == nil {
		return
	}
	p.state, p.title = taskLineTitle(b)
	p.ended = b.Done
}

// settleTaskFeeds starts the loads the task feeds ask for, and makes sure a
// flush follows when a missing step is due for a reload.
func (s *screen) settleTaskFeeds() {
	now := s.now()
	var next time.Duration
	for _, id := range s.panes.leaves() {
		p := s.panes.get(id)
		if p == nil || p.feed == nil {
			continue
		}
		if wait := p.feed.checkGaps(now); wait > 0 && (next == 0 || wait < next) {
			next = wait
		}
		for _, l := range p.feed.takeLoads() {
			s.loadTask(p, l)
		}
	}
	if next > 0 && !s.gapCheck {
		s.gapCheck = true
		time.AfterFunc(next, func() { s.post(taskGapEvent{at: time.Now()}) })
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

// now is the time for the task feeds' gap wait.
func (s *screen) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
