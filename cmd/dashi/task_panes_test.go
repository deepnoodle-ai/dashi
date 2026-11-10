package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/tui"
	"github.com/deepnoodle-ai/wonton/tui/tuitest"
)

// fakeTaskSource stands in for the Engine's task seam. Its watch yields the
// record, then each event send delivers, in the order sent: ordering is
// delegate.Tasks.Watch's job, tested there.
type fakeTaskSource struct {
	mu        sync.Mutex
	live      map[string]chan dashi.Event
	unwatched int
	calls     []string
	record    func(id string) ([]dashi.Turn, error)
	// facts, if set, replaces the running snapshot and gets the load's
	// context.
	facts func(ctx context.Context, id string) (taskSnapshot, error)
}

func (f *fakeTaskSource) channel(id string) chan dashi.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = map[string]chan dashi.Event{}
	}
	if f.live[id] == nil {
		f.live[id] = make(chan dashi.Event, 1024)
	}
	return f.live[id]
}

func (f *fakeTaskSource) WatchTask(ctx context.Context, id string) iter.Seq2[delegate.TaskUpdate, error] {
	return func(yield func(delegate.TaskUpdate, error) bool) {
		defer func() {
			f.mu.Lock()
			f.unwatched++
			f.mu.Unlock()
		}()
		f.mu.Lock()
		f.calls = append(f.calls, "watch "+id)
		record := f.record
		f.mu.Unlock()
		live := f.channel(id)
		var turns []dashi.Turn
		err := errors.New("no record")
		if record != nil {
			turns, err = record(id)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !yield(delegate.TaskUpdate{Kind: delegate.TaskUpdateMissingSteps, Err: err}, nil) {
			return
		}
		for i := range turns {
			if !yield(delegate.TaskUpdate{Kind: delegate.TaskUpdateTurn, Turn: &turns[i]}, nil) {
				return
			}
		}
		if !yield(delegate.TaskUpdate{Kind: delegate.TaskUpdateRecordEnd}, nil) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-live:
				if !yield(delegate.TaskUpdate{Kind: delegate.TaskUpdateEvent, Event: ev}, nil) {
					return
				}
			}
		}
	}
}

func (f *fakeTaskSource) TaskFacts(ctx context.Context, id string) (taskSnapshot, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "facts "+id)
	facts := f.facts
	f.mu.Unlock()
	if facts != nil {
		return facts(ctx, id)
	}
	return taskSnapshot{Task: runningTask(id), Bound: true}, nil
}

// send delivers a live event to the watch of task id.
func (f *fakeTaskSource) send(id string, ev dashi.Event) {
	f.channel(id) <- ev
}

// waitUnwatched waits until n watches have ended.
func (f *fakeTaskSource) waitUnwatched(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := f.unwatched
		f.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unwatched=%d, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitLoad waits until a facts load result is queued and every task pane
// has its record, then drains.
func waitLoad(t *testing.T, s *screen) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.pendMu.Lock()
		ok := slices.ContainsFunc(s.pending, func(q queued) bool { return q.load != nil })
		s.pendMu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no load result queued")
		}
		time.Sleep(time.Millisecond)
	}
	s.drain()
	waitRecords(t, s)
}

// waitRecords drains until every task pane has its record.
func waitRecords(t *testing.T, s *screen) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.drain()
		loading := false
		for _, id := range s.panes.leaves() {
			if p := s.panes.get(id); p != nil && p.watch != nil && p.watch.loading {
				loading = true
			}
		}
		if !loading {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a task pane never got its record")
		}
		time.Sleep(time.Millisecond)
	}
}

// drainUntil drains until cond holds.
func drainUntil(t *testing.T, s *screen, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.drain()
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func runningTask(id string) delegate.TaskView {
	return delegate.TaskView{ID: id, Subagent: "explore", State: delegate.TaskRunning, ModelCalls: 2, Description: "Find Config.MaxReply usage\nThen report."}
}

// taskPaneFixture is the visual fixture with a fake task source whose
// record is turn, running.
func taskPaneFixture(t *testing.T, width, height int, turn dashi.Turn) (*screen, *fakeTaskSource) {
	t.Helper()
	s := terminalVisualFixture(t, "success", width, height)
	src := &fakeTaskSource{record: func(string) ([]dashi.Turn, error) {
		return []dashi.Turn{turn}, nil
	}}
	s.tasks = src
	return s, src
}

func TestTaskPaneShowsRecordThenLive(t *testing.T) {
	turn := recordedChildTurn(t)
	partial := dashi.Turn{ID: turn.ID, Steps: turn.Steps[:3]}
	s, src := taskPaneFixture(t, 120, 40, partial)
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	if p == nil || s.panes.active != p.id {
		t.Fatal("the task pane did not open and activate")
	}
	waitLoad(t, s)
	for _, ev := range stepsOf(turn)[3:] {
		src.send("t_1", ev)
	}
	drainUntil(t, s, "the stop step", func() bool { return strings.Contains(paneText(p.watch), "── t_1 turn") })
	if got, want := paneText(p.watch), wantText(t, p.watch.task, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	// The pane's header is the description's first line, not the facts.
	if facts := p.view.tr.Blocks[0]; facts.ID != taskFactsID || facts.Kind != KindHint || facts.Text != "Find Config.MaxReply usage" {
		t.Fatalf("facts=%+v", facts)
	}
	if p.state != "running" || p.title != "2 model calls" || p.subagent != "explore" {
		t.Fatalf("state=%q title=%q subagent=%q", p.state, p.title, p.subagent)
	}
	// The stop step asked for fresh facts.
	waitLoad(t, s)
	// Opening again focuses the pane and refreshes the facts.
	s.activatePane(mainPaneID)
	s.openTask("t_1")
	if s.panes.active != p.id || s.panes.count() != 2 {
		t.Fatal("a second open did not focus the open pane")
	}
	waitLoad(t, s)
}

func TestTaskPaneFactsLoadTimesOut(t *testing.T) {
	turn := recordedChildTurn(t)
	s, src := taskPaneFixture(t, 120, 40, turn)
	s.taskTimeout = 20 * time.Millisecond
	src.facts = func(ctx context.Context, _ string) (taskSnapshot, error) {
		<-ctx.Done()
		return taskSnapshot{}, ctx.Err()
	}
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	waitLoad(t, s)
	if p.state != "record unavailable" {
		t.Fatalf("state=%q", p.state)
	}
	facts := p.view.tr.Blocks[0]
	if facts.Kind != KindWarning || !strings.Contains(facts.Text, "the load took longer than 20ms") {
		t.Fatalf("facts=%+v", facts)
	}
}

func TestTaskPaneRecordFailureShowsWarningAndGoesLive(t *testing.T) {
	turn := recordedChildTurn(t)
	s, src := taskPaneFixture(t, 120, 40, turn)
	src.record = func(string) ([]dashi.Turn, error) { return nil, errors.New("record locked") }
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	waitLoad(t, s)
	i, ok := p.view.tr.index[taskRecordID]
	if !ok || p.view.tr.Blocks[i].Kind != KindWarning || !strings.Contains(p.view.tr.Blocks[i].Text, "record locked") {
		t.Fatalf("blocks=%+v", p.view.tr.Blocks)
	}
	for _, ev := range stepsOf(turn) {
		src.send("t_1", ev)
	}
	drainUntil(t, s, "the live turn", func() bool { return strings.Contains(paneText(p.watch), "── t_1 turn") })
}

func TestTaskPaneCloseDropsQueuedUpdatesAndStaleLoads(t *testing.T) {
	turn := recordedChildTurn(t)
	s, src := taskPaneFixture(t, 120, 40, turn)
	release := make(chan struct{})
	src.record = func(string) ([]dashi.Turn, error) {
		<-release
		return []dashi.Turn{turn}, nil
	}
	s.openTask("t_1")
	s.closePane(taskPaneID("t_1"))
	// Reopen: the old open's record arrives too and is dropped.
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	close(release)
	src.waitUnwatched(t, 1)
	waitRecords(t, s)
	if p.watch.token != 2 {
		t.Fatalf("token=%d", p.watch.token)
	}
	s.pendMu.Lock()
	stale := slices.ContainsFunc(s.pending, func(q queued) bool { return q.update != nil && q.update.token == 1 })
	s.pendMu.Unlock()
	if stale {
		t.Fatal("closing kept the pane's queued updates")
	}
	// A late load of the closed open is dropped too.
	s.taskFactsLoaded(taskFactsLoad{open: true, pane: p.id, token: 1, err: errors.New("stale")})
	if p.view.tr.Blocks[0].Kind == KindWarning {
		t.Fatal("a stale load reached the reopened pane")
	}
	// A late update of the closed open is dropped.
	s.taskUpdate(taskWatchUpdate{pane: p.id, token: 1, update: delegate.TaskUpdate{Kind: delegate.TaskUpdateEvent, Event: stepsOf(turn)[0]}})
	if got, want := paneText(p.watch), wantText(t, p.watch.task, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	// Reset closes every side pane, which ends its watch.
	s.Reset()
	s.drain()
	if s.panes.count() != 1 {
		t.Fatalf("panes=%d", s.panes.count())
	}
	src.waitUnwatched(t, 2)
}

// recordedChildTurn returns a real recorded Turn with a tool call between
// two model calls, to stand in for a child Turn.
func recordedChildTurn(t *testing.T) dashi.Turn {
	t.Helper()
	h := newHarness(t, callTool("c1", "read_file", `{"path":"notes.txt"}`), say("The notes say hello."))
	if err := os.WriteFile(filepath.Join(h.dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.input("Read the notes")
	turn := h.turn(1)
	if len(turn.Steps) < 6 {
		t.Fatalf("turn has %d steps", len(turn.Steps))
	}
	return turn
}

// recordedChildTurns returns two real recorded Turns of one session, each
// with a tool call between two model calls, for a task with two turns.
func recordedChildTurns(t *testing.T) (dashi.Turn, dashi.Turn) {
	t.Helper()
	h := newHarness(t, callTool("c1", "read_file", `{"path":"notes.txt"}`), say("The notes say hello."),
		callTool("c2", "read_file", `{"path":"notes.txt"}`), say("Still hello."))
	if err := os.WriteFile(filepath.Join(h.dir, "notes.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.input("Read the notes")
	h.input("Read them again")
	return h.turn(1), h.turn(2)
}

func newTestWatch() *taskWatch {
	view := newTranscriptPane("task:t_1", func() int { return 80 }, func() string { return "" })
	return newTaskWatch("t_1", 1, "/repo", view)
}

func stepsOf(turn dashi.Turn) []dashi.Event {
	var out []dashi.Event
	for i := range turn.Steps {
		s := turn.Steps[i]
		out = append(out, dashi.Event{TurnID: turn.ID, Seq: s.Seq, Step: &s})
	}
	return out
}

func eventUpdate(ev dashi.Event) delegate.TaskUpdate {
	return delegate.TaskUpdate{Kind: delegate.TaskUpdateEvent, Event: ev}
}

func turnUpdate(turn dashi.Turn) delegate.TaskUpdate {
	return delegate.TaskUpdate{Kind: delegate.TaskUpdateTurn, Turn: &turn}
}

// paneText is the plain text of a pane's blocks after the facts block.
func paneText(w *taskWatch) string {
	var lines []string
	for _, b := range w.view.tr.Blocks {
		if b.ID == taskFactsID {
			continue
		}
		lines = append(lines, FormatBlock(b))
	}
	return strings.Join(lines, "\n")
}

// wantText is what the pane of task shows for the whole turn: what
// RenderTurn shows for a reopened session, with the task in the footer.
func wantText(t *testing.T, task string, turn dashi.Turn) string {
	t.Helper()
	blocks, err := RenderTurn(turnNumber(turn.ID), "", "/repo", turn, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	for _, b := range blocks {
		if b.Footer != nil {
			f := *b.Footer
			f.Task = task
			b.Footer, b.Text = &f, f.String()
		}
		tr.Apply(b)
	}
	var lines []string
	for _, b := range tr.Blocks {
		lines = append(lines, FormatBlock(b))
	}
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "── turn") {
		t.Fatalf("a footer lacks the task:\n%s", text)
	}
	return text
}

// A recorded part of a Turn, then its live steps, show what a reopened
// session shows for the whole Turn.
func TestTaskWatchReplaysRecordThenLiveSteps(t *testing.T) {
	turn := recordedChildTurn(t)
	w := newTestWatch()
	w.apply(turnUpdate(dashi.Turn{ID: turn.ID, Steps: turn.Steps[:4]}), nil)
	w.apply(delegate.TaskUpdate{Kind: delegate.TaskUpdateRecordEnd}, nil)
	if w.loading {
		t.Fatal("still loading after the record")
	}
	for _, ev := range stepsOf(turn)[4:] {
		w.apply(eventUpdate(ev), nil)
	}
	if got, want := paneText(w), wantText(t, w.task, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	if !w.factsDue {
		t.Fatal("the stop step did not ask for fresh facts")
	}
}

func TestTaskWatchShowsTwoTurns(t *testing.T) {
	one, two := recordedChildTurns(t)
	w := newTestWatch()
	w.apply(turnUpdate(one), nil)
	w.apply(turnUpdate(dashi.Turn{ID: two.ID, Steps: two.Steps[:2]}), nil)
	for _, ev := range stepsOf(two)[2:] {
		w.apply(eventUpdate(ev), nil)
	}
	want := wantText(t, w.task, one) + "\n" + wantText(t, w.task, two)
	if got := paneText(w); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
}

func TestTaskWatchPreviewsAndGaps(t *testing.T) {
	turn := recordedChildTurn(t)
	events := stepsOf(turn)
	w := newTestWatch()
	var started, finished dashi.Event
	for _, ev := range events {
		if ev.Step.Model != nil && ev.Step.Kind == dashi.StepModelCallStarted {
			started = ev
		}
		if ev.Step.Model != nil && ev.Step.Kind == dashi.StepModelCallFinished {
			finished = ev
		}
	}
	m := finished.Step.Model
	key := messageKey(m.MessageID, m.ModelCallID)
	for _, ev := range events[:started.Seq] {
		w.apply(eventUpdate(ev), nil)
	}
	w.apply(eventUpdate(dashi.Event{TurnID: turn.ID, Seq: started.Seq, ModelCallID: m.ModelCallID, MessageID: m.MessageID, Delta: &llm.Delta{Kind: llm.DeltaText, Text: "The notes"}}), nil)
	if i, ok := w.view.tr.index[key]; !ok || w.view.tr.Blocks[i].Text != "The notes" {
		t.Fatal("the preview did not show")
	}
	// A gap drops the preview text.
	w.apply(eventUpdate(dashi.Event{TurnID: turn.ID, Seq: started.Seq, ModelCallID: m.ModelCallID, MessageID: m.MessageID, Gap: true}), nil)
	if i := w.view.tr.index[key]; w.view.tr.Blocks[i].Text != "" {
		t.Fatalf("gap kept preview text %q", w.view.tr.Blocks[i].Text)
	}
	for _, ev := range events[started.Seq:] {
		w.apply(eventUpdate(ev), nil)
	}
	if got, want := paneText(w), wantText(t, w.task, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
}

func TestTaskWatchShowsMissingSteps(t *testing.T) {
	turn := recordedChildTurn(t)
	w := newTestWatch()
	w.apply(delegate.TaskUpdate{Kind: delegate.TaskUpdateMissingSteps, Err: errors.New("record locked")}, nil)
	w.apply(eventUpdate(stepsOf(turn)[0]), nil)
	w.apply(delegate.TaskUpdate{Kind: delegate.TaskUpdateMissingSteps, TurnID: turn.ID, Err: errors.New("disk gone")}, nil)
	text := paneText(w)
	if !strings.Contains(text, "Task record unavailable: record locked") || !strings.Contains(text, "Turn 1 is missing steps (disk gone)") {
		t.Fatalf("pane:\n%s", text)
	}
	w.apply(delegate.TaskUpdate{}, errors.New("delegate: unknown task"))
	if i := w.view.tr.index[taskRecordID]; !strings.Contains(w.view.tr.Blocks[i].Text, "unknown task") {
		t.Fatal("a watch error did not show")
	}
}

func TestTaskPaneTitleFollowsStatusLinesAndApprovals(t *testing.T) {
	turn := recordedChildTurn(t)
	child := delegate.ChildID("parent-1", "call-1")
	id := delegate.TaskID(child)
	s, _ := taskPaneFixture(t, 120, 40, turn)
	s.openTask(id)
	waitLoad(t, s)
	p := s.panes.get(taskPaneID(id))
	v := delegate.TaskView{ID: id, Subagent: "explore", State: delegate.TaskRunning, ModelCalls: 3, Parent: dashi.CallRef{TurnID: "parent-1", CallID: "call-1"}}
	s.Emit(taskStatusBlock(v))
	s.drain()
	if p.state != "running" || p.title != "3 model calls" || p.ended {
		t.Fatalf("state=%q title=%q ended=%v", p.state, p.title, p.ended)
	}
	rows := frameText(s)
	if !strings.Contains(rows, activeMark+" "+id+" · running · 3 model calls") {
		t.Fatalf("running title not drawn:\n%s", rows)
	}
	ax, ay := cellOf(t, strings.Split(rows, "\n"), id+" · running")
	accent := tuitest.Screen(s.View(), tui.WithWidth(120), tui.WithHeight(40)).Cell(ax, ay).Style.Foreground
	// A prompt of the task, active or queued, marks it.
	s.ctrl.broker.mu.Lock()
	s.ctrl.broker.queued = map[brokerOwner]int{{TaskID: child}: 1}
	s.ctrl.broker.mu.Unlock()
	s.HandleEvent(tui.TickEvent{Frame: 1})
	if !p.waiting {
		t.Fatal("a queued prompt of the task did not mark it")
	}
	frame := tuitest.Screen(s.View(), tui.WithWidth(120), tui.WithHeight(40))
	x, y := cellOf(t, strings.Split(frame.Text(), "\n"), id+" · needs approval · 3 model calls")
	if frame.Cell(x, y).Style.Foreground == accent {
		t.Fatal("needs approval is not in the warning color")
	}
	// Narrow mode says it on the tab.
	s.width = 80
	if rows := frameText(s); !strings.Contains(rows, id+" needs approval") {
		t.Fatalf("narrow tab:\n%s", rows)
	}
	s.width = 120
	s.ctrl.broker.mu.Lock()
	s.ctrl.broker.queued = nil
	s.ctrl.broker.mu.Unlock()
	s.clearPrompts()
	if p.waiting {
		t.Fatal("waiting stayed after the prompt ended")
	}
	// The task ends: the pane stays with the final state.
	v.State, v.ModelCalls = delegate.TaskState(dashi.StopCompleted), 4
	s.Emit(taskStatusBlock(v))
	s.drain()
	if !p.ended || !strings.Contains(frameText(s), id+" · completed · 4 model calls") {
		t.Fatalf("ended title=%q", p.title)
	}
}

func TestTaskLineOpensPaneByClickAndMenu(t *testing.T) {
	turn := recordedChildTurn(t)
	s, _ := taskPaneFixture(t, 120, 40, turn)
	v := delegate.TaskView{ID: "t_k3f2", TurnID: "child-1", Subagent: "explore", State: delegate.TaskRunning, Parent: dashi.CallRef{TurnID: "parent-1", CallID: "tool"}}
	s.Emit(Block{Kind: KindTool, ID: "t:tool", Name: "task", Arg: "explore · find the parser", State: StateRunning})
	s.Emit(taskStatusBlock(v))
	s.drain()
	rows := strings.Split(frameText(s), "\n")
	if strings.Contains(strings.Join(rows, "\n"), "to inspect") || strings.Contains(strings.Join(rows, "\n"), "›") {
		t.Fatalf("fullscreen task line keeps the inspect hint or the old mark:\n%s", strings.Join(rows, "\n"))
	}
	// The record keeps the inspect hint that inline mode prints.
	var line Block
	for _, b := range s.main().tr.Blocks {
		if b.Kind == KindTool && b.Task != nil {
			line = *b.Task
		}
	}
	if !strings.Contains(FormatBlock(line), "/task t_k3f2 to inspect") {
		t.Fatal("the fullscreen hint entered the line's text")
	}
	// The tool row's head does not open the task.
	x, y := cellOf(t, rows, "Starting a subagent")
	click(s, tui.MouseButtonLeft, x, y)
	if s.panes.count() != 1 {
		t.Fatal("a click on the tool row opened a pane")
	}
	x, y = cellOf(t, rows, "click to watch")
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonRight, ClickCount: 1, X: x, Y: y})
	var labels []string
	for _, item := range s.transcriptMenu.Items() {
		labels = append(labels, item.Label)
	}
	if !slices.Contains(labels, "Open task") {
		t.Fatalf("menu=%v", labels)
	}
	s.transcriptMenu.Close()
	click(s, tui.MouseButtonLeft, x, y)
	if s.panes.get(taskPaneID("t_k3f2")) == nil || s.panes.active != taskPaneID("t_k3f2") {
		t.Fatal("a click on the task line did not open its pane")
	}
	waitLoad(t, s)
	if got := frameText(s); !strings.Contains(got, "watching") || strings.Contains(got, "click to watch") {
		t.Fatalf("an open pane's task line does not say watching:\n%s", got)
	}
	s.closePane(taskPaneID("t_k3f2"))
	if got := frameText(s); !strings.Contains(got, "click to watch") {
		t.Fatalf("a closed pane's task line does not offer to watch:\n%s", got)
	}
}

func TestTaskPaneFramesAtNarrowWidth(t *testing.T) {
	turn := recordedChildTurn(t)
	s, _ := taskPaneFixture(t, 80, 24, turn)
	s.openTask("t_1")
	waitLoad(t, s)
	rows := frameText(s)
	for _, want := range []string{"  main  t_1 ", "The notes say hello.", "Find Config.MaxReply usage", "→ main"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("missing %q:\n%s", want, rows)
		}
	}
	s.width, s.height = 40, 16
	if rows := frameText(s); !strings.Contains(rows, " t_1 ") {
		t.Fatalf("40x16:\n%s", rows)
	}
}

// loopScreen runs a screen's event loop on a goroutine, as the runtime
// does, and returns a function that runs code on it.
func loopScreen(t *testing.T, s *screen) func(func()) {
	t.Helper()
	var mu sync.Mutex
	events := make(chan tui.Event, 1000)
	var sendMu sync.Mutex
	closed := false
	s.pendMu.Lock()
	s.send = func(e tui.Event) {
		sendMu.Lock()
		defer sendMu.Unlock()
		if !closed {
			events <- e
		}
	}
	s.pendMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			mu.Lock()
			s.HandleEvent(e)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		sendMu.Lock()
		closed = true
		close(events)
		sendMu.Unlock()
		<-done
	})
	return func(fn func()) {
		mu.Lock()
		defer mu.Unlock()
		fn()
	}
}

func TestTaskPaneStreamsARealChildOpenedMidTurn(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	client := &taskFixtureClient{childHold: hold, childStarted: started, loopUntil: 2}
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ApproveAll = true
	s := newScreen(cfg.Dir)
	s.width, s.height = 120, 40
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = false
	a.ctrl.mu.Unlock()
	on := loopScreen(t, s)
	t.Cleanup(func() { a.ctrl.CancelWork(); a.eng.closeTasks(); a.ctrl.Wait(); a.eng.Close() })
	if _, err := a.eng.Prompt(t.Context(), "Delegate this job"); err != nil {
		t.Fatal(err)
	}
	<-started
	task := a.eng.taskHost().tasks.List()[0]
	// A bare /task ID opens the pane mid-turn.
	on(func() { s.submit("/task " + task.ID) })
	pane := taskPaneID(task.ID)
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			ok := false
			on(func() { s.drain(); ok = cond() })
			if ok {
				return
			}
			if time.Now().After(deadline) {
				var text string
				on(func() {
					if p := s.panes.get(pane); p != nil {
						text = paneText(p.watch)
					}
				})
				t.Fatalf("timeout waiting for %s; pane:\n%s", what, text)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait("the record", func() bool { p := s.panes.get(pane); return p != nil && !p.watch.loading })
	close(hold)
	wait("the child's end", func() bool {
		p := s.panes.get(pane)
		return strings.Contains(paneText(p.watch), "── "+task.ID+" turn 1") && p.ended
	})
	var text, title string
	on(func() { p := s.panes.get(pane); text, title = paneText(p.watch), p.state })
	if strings.Count(text, "Child result, recorded once.") != 1 || strings.Count(text, "── "+task.ID+" turn 1") != 1 {
		t.Fatalf("pane:\n%s", text)
	}
	if strings.Count(text, "list_directory") != 2 || !strings.HasPrefix(text, "> Inspect the focused task") {
		t.Fatalf("pane:\n%s", text)
	}
	if !strings.Contains(title, "completed") {
		t.Fatalf("title=%q", title)
	}
	// /tasks ID shows the full facts above the turns in the details pane.
	on(func() { s.submit("/tasks " + task.ID) })
	wait("the task details", func() bool {
		got := detailsText(s)
		return s.panes.active == detailsPaneID && strings.Contains(got, "Child Turn:") && strings.Contains(got, "Child result, recorded once.")
	})
	// /tasks lists the tasks there, then adds the spend under the list.
	on(func() { s.submit("/tasks") })
	wait("the task list and spend", func() bool {
		got := detailsText(s)
		return strings.Contains(got, "Tasks:\n  "+task.ID) && strings.Contains(got, "spend (live lower bounds")
	})
	// A subcommand keeps its behavior and does not move focus.
	on(func() { s.activatePane(mainPaneID); s.submit("/task " + task.ID + " discard") })
	time.Sleep(20 * time.Millisecond)
	on(func() {
		if s.panes.active != mainPaneID {
			t.Error("a /task subcommand moved focus")
		}
	})
}

func TestBrokerWaitingOwnersIncludesQueuedPrompts(t *testing.T) {
	shown := make(chan brokerPrompt, 2)
	b := newApprovalBroker(func(p brokerPrompt) <-chan struct{} { shown <- p; return nil }, nil)
	first := brokerOwner{TaskID: "child-a", Call: dashi.CallRef{TurnID: "a-1", CallID: "c1"}}
	second := brokerOwner{TaskID: "child-b", Call: dashi.CallRef{TurnID: "b-1", CallID: "c2"}}
	go b.run(t.Context(), brokerPrompt{Owner: first, Approval: &ApprovalRequest{}})
	p := <-shown
	go b.run(t.Context(), brokerPrompt{Owner: second, Approval: &ApprovalRequest{}})
	waitOwners := func(n int) []brokerOwner {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			owners := b.waitingOwners()
			if len(owners) == n {
				return owners
			}
			if time.Now().After(deadline) {
				t.Fatalf("owners=%v, want %d", owners, n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if owners := waitOwners(2); !slices.Contains(owners, first) || !slices.Contains(owners, second) {
		t.Fatalf("owners=%v", owners)
	}
	b.respond(p.ID, first, brokerResponse{})
	p = <-shown
	if owners := waitOwners(1); owners[0] != second {
		t.Fatalf("owners=%v", owners)
	}
	b.respond(p.ID, second, brokerResponse{})
	waitOwners(0)
}

// The first task line of a session shows a one-time hint about watching
// tasks; a new session shows it again.
func TestFirstTaskLineShowsTheWatchTipOnce(t *testing.T) {
	s := terminalVisualFixture(t, "success", 120, 40)
	count := func() int {
		n := 0
		for _, b := range s.main().tr.Blocks {
			if strings.HasPrefix(b.Text, "Click a task, or type /task t_") {
				if b.Kind != KindHint {
					t.Fatalf("tip kind=%s", b.Kind)
				}
				n++
			}
		}
		return n
	}
	s.Emit(taskStatusBlock(runningTask("t_1")))
	s.Emit(taskStatusBlock(runningTask("t_1")))
	s.Emit(taskStatusBlock(runningTask("t_2")))
	s.drain()
	if count() != 1 {
		t.Fatalf("tip shown %d times", count())
	}
	if !strings.Contains(dialogTranscriptText(s), "type /task t_1, to watch") {
		t.Fatal("the tip does not name the task")
	}
	s.Reset()
	s.drain()
	s.Emit(taskStatusBlock(runningTask("t_3")))
	s.drain()
	if count() != 1 {
		t.Fatalf("a new session showed the tip %d times", count())
	}
}

// While an approval waits, keys stay with it, but the pointer still
// reaches the panes: a click on a task line opens its pane, the wheel
// scrolls the pane under it, a title right-click opens its menu, and the
// wheel elsewhere scrolls the dialog.
func TestPointerReachesPanesDuringAnApproval(t *testing.T) {
	turn := recordedChildTurn(t)
	s, _ := taskPaneFixture(t, 120, 40, turn)
	v := delegate.TaskView{ID: "t_k3f2", TurnID: "child-1", Subagent: "explore", State: delegate.TaskRunning, Parent: dashi.CallRef{TurnID: "parent-1", CallID: "tool"}}
	s.Emit(Block{Kind: KindTool, ID: "t:tool", Name: "task", Arg: "explore · find the parser", State: StateRunning})
	s.Emit(taskStatusBlock(v))
	for i := range 60 {
		s.Emit(Block{ID: fmt.Sprintf("m%d", i), Kind: KindAssistant, Text: fmt.Sprintf("main line %d", i), Done: true})
	}
	s.drain()
	s.main().vp.ScrollToTop()
	s.approval = &approvalPrompt{reply: make(chan Approval, 1)}
	rows := strings.Split(frameText(s), "\n")
	x, y := cellOf(t, rows, "click to watch")
	click(s, tui.MouseButtonLeft, x, y)
	if s.panes.get(taskPaneID("t_k3f2")) == nil || s.approval == nil {
		t.Fatal("a click on a task line during an approval did not open its pane")
	}
	waitLoad(t, s)
	rows = strings.Split(frameText(s), "\n")
	// The wheel over main scrolls main, not the dialog.
	mx, my := cellOf(t, rows, "main line 0")
	before, _ := s.main().vp.Anchor()
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseScroll, Button: tui.MouseButtonWheelDown, X: mx, Y: my})
	frameText(s)
	if after, _ := s.main().vp.Anchor(); after == before {
		t.Fatal("the wheel over main did not scroll it during an approval")
	}
	// A right-click on the task pane's title opens its menu.
	tx, ty := cellOf(t, rows, "t_k3f2 · running")
	click(s, tui.MouseButtonRight, tx, ty)
	if items := s.transcriptMenu.Items(); len(items) == 0 || items[0].Label != "Task details" {
		t.Fatalf("title menu during an approval=%v", items)
	}
	s.transcriptMenu.Close()
	// Pane keys stay swallowed.
	active := s.panes.active
	s.HandleEvent(inputActionEvent{key: altKey(tui.KeyArrowLeft, 0)})
	if s.panes.active != active {
		t.Fatal("Alt+Left moved focus during an approval")
	}
	// The × closes the pane.
	cx, cy := cellOf(t, rows, "×")
	click(s, tui.MouseButtonLeft, cx, cy)
	if s.panes.get(taskPaneID("t_k3f2")) != nil {
		t.Fatal("the × did not close the pane")
	}
}

// A task_send message starts the task's next Turn, which streams into the
// pane already open after the first. After a restart, a new launch has only
// the main pane, and the reopened task's pane shows each recorded Turn
// once (UC-1, UC-7).
func TestTaskPaneFollowsTaskSendAndReopensFromTheRecord(t *testing.T) {
	const up, down = "Parse is in config/load.go; env overrides too?", "Yes, check env overrides too"
	hold := make(chan struct{})
	started := make(chan struct{})
	client := &taskFixtureClient{childHold: hold, childStarted: started, messageParent: up, messageDeliver: "queue", sendQueue: down}
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ApproveAll = true
	launch := func(opts Options) (*app, *screen, func(func())) {
		s := newScreen(cfg.Dir)
		s.width, s.height = 120, 40
		a, err := buildApp(t.Context(), cfg, s)
		if err != nil {
			t.Fatal(err)
		}
		s.bind(a)
		if _, err = a.openSession(cfg.Store, cfg.Dir, opts); err != nil {
			t.Fatal(err)
		}
		a.ctrl.mu.Lock()
		a.ctrl.autoTaskDelivery = false
		a.ctrl.mu.Unlock()
		return a, s, loopScreen(t, s)
	}
	a, s, on := launch(Options{})
	closed := false
	t.Cleanup(func() {
		if !closed {
			a.ctrl.CancelWork()
			a.eng.closeTasks()
			a.ctrl.Wait()
			a.eng.Close()
		}
	})
	wait := func(on func(func()), what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			ok := false
			on(func() { ok = cond() })
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	task := a.eng.taskHost().tasks.List()[0]
	pane := taskPaneID(task.ID)
	on(func() { s.submit("/task " + task.ID) })
	wait(on, "the record", func() bool { s.drain(); p := s.panes.get(pane); return p != nil && !p.watch.loading })
	close(hold)
	if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	waitTask(t, a, func(v delegate.TaskView) bool { return v.Turns == 2 && !v.Running() })
	turnRule := func(n int) string { return fmt.Sprintf("── %s turn %d ", task.ID, n) }
	check := func(text string) {
		t.Helper()
		for _, want := range []string{turnRule(1), turnRule(2), down} {
			if strings.Count(text, want) != 1 {
				t.Fatalf("%q shows %d times:\n%s", want, strings.Count(text, want), text)
			}
		}
		if strings.Index(text, turnRule(1)) > strings.Index(text, down) {
			t.Fatalf("turn 2 is not after turn 1:\n%s", text)
		}
	}
	var text string
	wait(on, "turn 2 in the pane", func() bool {
		s.drain()
		text = paneText(s.panes.get(pane).watch)
		return strings.Contains(text, turnRule(2))
	})
	check(text)

	// Restart: the next launch starts with only main, and the task's pane
	// replays both Turns from the record.
	sessionID := a.eng.Session().ID
	a.ctrl.CancelWork()
	a.eng.closeTasks()
	a.ctrl.Wait()
	a.eng.Close()
	closed = true
	b, s2, on2 := launch(Options{Resume: true, ResumeID: sessionID})
	t.Cleanup(func() { b.ctrl.CancelWork(); b.eng.closeTasks(); b.ctrl.Wait(); b.eng.Close() })
	// Startup recovers the session's tasks, as the MCP start does.
	if err := b.eng.recoverTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	on2(func() {
		if s2.panes.count() != 1 {
			t.Errorf("a new launch opened %d panes", s2.panes.count())
		}
	})
	on2(func() { s2.submit("/task " + task.ID) })
	wait(on2, "the reopened record", func() bool {
		s2.drain()
		p := s2.panes.get(pane)
		if p == nil || p.watch.loading {
			return false
		}
		text = paneText(p.watch)
		return strings.Contains(text, turnRule(2))
	})
	check(text)
}
