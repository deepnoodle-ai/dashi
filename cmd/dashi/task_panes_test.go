package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/wonton/tui"
)

// fakeTaskSource stands in for the Engine's task seam.
type fakeTaskSource struct {
	mu        sync.Mutex
	watchers  map[string]func(delegate.ChildEvent)
	unwatched int
	calls     []string
	record    func(id string) (taskSnapshot, []dashi.Turn, error)
}

func (f *fakeTaskSource) WatchTask(id string, fn func(delegate.ChildEvent)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchers == nil {
		f.watchers = map[string]func(delegate.ChildEvent){}
	}
	f.watchers[id] = fn
	f.calls = append(f.calls, "watch "+id)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unwatched++
	}
}

func (f *fakeTaskSource) TaskRecord(_ context.Context, id string) (taskSnapshot, []dashi.Turn, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "load "+id)
	record := f.record
	f.mu.Unlock()
	if record == nil {
		return taskSnapshot{}, nil, errors.New("no record")
	}
	return record(id)
}

// send delivers a child event the way OnChild does.
func (f *fakeTaskSource) send(id string, ev dashi.Event) {
	f.mu.Lock()
	fn := f.watchers[id]
	f.mu.Unlock()
	fn(delegate.ChildEvent{Subagent: "explore", Event: ev})
}

// waitLoad waits until a load result is queued, then drains it.
func waitLoad(t *testing.T, s *screen) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.pendMu.Lock()
		ok := slices.ContainsFunc(s.pending, func(q queued) bool { return q.load != nil })
		s.pendMu.Unlock()
		if ok {
			s.drain()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no load result queued")
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
	src := &fakeTaskSource{record: func(id string) (taskSnapshot, []dashi.Turn, error) {
		return taskSnapshot{Task: runningTask(id), Bound: true}, []dashi.Turn{turn}, nil
	}}
	s.tasks = src
	return s, src
}

func TestTaskPaneWatchesBeforeLoadingAndShowsRecordThenLive(t *testing.T) {
	turn := recordedChildTurn(t)
	partial := dashi.Turn{ID: turn.ID, Steps: turn.Steps[:3]}
	s, src := taskPaneFixture(t, 120, 40, partial)
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	if p == nil || s.panes.active != p.id {
		t.Fatal("the task pane did not open and activate")
	}
	// Live steps arrive before the record has been applied.
	for _, ev := range stepsOf(turn)[1:] {
		src.send("t_1", ev)
	}
	waitLoad(t, s)
	if got := strings.Join(src.calls, ","); got != "watch t_1,load t_1" {
		t.Fatalf("calls=%s", got)
	}
	got := strings.Join(strings.Split(paneText(p.feed), "\n"), "\n")
	if want := wantText(t, p.feed.task, turn); got != want {
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

func TestTaskPaneCloseDropsQueuedEventsAndStaleLoads(t *testing.T) {
	turn := recordedChildTurn(t)
	s, src := taskPaneFixture(t, 120, 40, turn)
	release := make(chan struct{})
	src.record = func(id string) (taskSnapshot, []dashi.Turn, error) {
		<-release
		return taskSnapshot{Task: runningTask(id)}, []dashi.Turn{turn}, nil
	}
	s.openTask("t_1")
	src.send("t_1", stepsOf(turn)[0])
	s.closePane(taskPaneID("t_1"))
	if src.unwatched != 1 {
		t.Fatal("closing did not unwatch")
	}
	s.pendMu.Lock()
	queuedChildren := slices.ContainsFunc(s.pending, func(q queued) bool { return q.child != nil })
	s.pendMu.Unlock()
	if queuedChildren {
		t.Fatal("closing kept the pane's queued events")
	}
	// Reopen: the old open's load result arrives first and is dropped.
	s.openTask("t_1")
	p := s.panes.get(taskPaneID("t_1"))
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for p.feed.replaying && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		s.drain()
	}
	if p.feed.token != 2 || p.feed.replaying {
		t.Fatalf("token=%d replaying=%v", p.feed.token, p.feed.replaying)
	}
	// A late load of the closed open is dropped too.
	s.taskLoaded(taskLoad{kind: taskLoadOpen, pane: p.id, token: 1, err: errors.New("stale")})
	if p.view.tr.Blocks[0].Kind == KindWarning {
		t.Fatal("a stale load reached the reopened pane")
	}
	// A late event of the closed open is dropped.
	s.taskChild(taskChildEvent{pane: p.id, token: 1, event: stepsOf(turn)[0]})
	if got, want := paneText(p.feed), wantText(t, p.feed.task, turn); got != want {
		t.Fatalf("pane:\n%s\nwant:\n%s", got, want)
	}
	// Reset closes every side pane, which unwatches.
	s.Reset()
	s.drain()
	if s.panes.count() != 1 || src.unwatched != 2 {
		t.Fatalf("panes=%d unwatched=%d", s.panes.count(), src.unwatched)
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
	accent := tui.SprintScreen(s.View(), tui.WithWidth(120), tui.WithHeight(40)).Cell(ax, ay).Style.Foreground
	// A prompt of the task, active or queued, marks it.
	s.ctrl.broker.mu.Lock()
	s.ctrl.broker.queued = map[brokerOwner]int{{TaskID: child}: 1}
	s.ctrl.broker.mu.Unlock()
	s.HandleEvent(tui.TickEvent{Frame: 1})
	if !p.waiting {
		t.Fatal("a queued prompt of the task did not mark it")
	}
	frame := tui.SprintScreen(s.View(), tui.WithWidth(120), tui.WithHeight(40))
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
						text = paneText(p.feed)
					}
				})
				t.Fatalf("timeout waiting for %s; pane:\n%s", what, text)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait("the record", func() bool { p := s.panes.get(pane); return p != nil && !p.feed.replaying })
	close(hold)
	wait("the child's end", func() bool {
		p := s.panes.get(pane)
		return strings.Contains(paneText(p.feed), "── "+task.ID+" turn 1") && p.ended
	})
	var text, title string
	on(func() { p := s.panes.get(pane); text, title = paneText(p.feed), p.state })
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
	wait(on, "the record", func() bool { s.drain(); p := s.panes.get(pane); return p != nil && !p.feed.replaying })
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
		text = paneText(s.panes.get(pane).feed)
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
		if p == nil || p.feed.replaying {
			return false
		}
		text = paneText(p.feed)
		return strings.Contains(text, turnRule(2))
	})
	check(text)
}
