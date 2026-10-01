package delegate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// noticeReminders counts the task-ended reminders in a model request.
func noticeReminders(in llm.Input) int {
	n := 0
	for _, m := range in.Messages {
		for _, c := range m.Content {
			if c.Reminder != nil && c.Reminder.Name == "task-ended" {
				n++
			}
		}
	}
	return n
}

// sessionTaskID is the ID of the task that turn 1 of session s started.
func sessionTaskID(s *session.Session) string {
	return delegate.TaskID(delegate.ChildID(session.TurnID(s.ID, 1), callID))
}

func sessionTurns(t *testing.T, s *session.Session) []dashi.Turn {
	t.Helper()
	var turns []dashi.Turn
	for turn, err := range s.Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	return turns
}

// A task_status call that gives the model a turn's final result delivers
// that result: no notice reaches the model, no queue turn runs for it, and
// a reopened host does not send it again. Each order of the child's end and
// the read must deliver once: the read waits and the child ends, the child
// ends before the read (its notice is withdrawn), or the two race.
func TestStatusReadOfTheFinalResultDeliversIt(t *testing.T) {
	type order struct {
		name  string
		runs  int
		first func(g *gate, ended <-chan delegate.TaskEvent) // before the read
	}
	orders := []order{
		{"read waits, then the child ends", 1, func(g *gate, _ <-chan delegate.TaskEvent) {
			go func() { time.Sleep(50 * time.Millisecond); g.open() }()
		}},
		{"child ends, then the read", 1, func(g *gate, ended <-chan delegate.TaskEvent) {
			g.open()
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
			}
		}},
		{"race", 10, func(g *gate, _ <-chan delegate.TaskEvent) { go g.open() }},
	}
	for _, o := range orders {
		for i := range o.runs {
			t.Run(fmt.Sprintf("%s/%d", o.name, i), func(t *testing.T) {
				testStatusReadDelivers(t, o.first, o.name)
			})
		}
	}
}

func testStatusReadDelivers(t *testing.T, first func(*gate, <-chan delegate.TaskEvent), name string) {
	g := newGate()
	var notices, parentCalls atomic.Int32
	var s *session.Session
	var h *harness
	ended := make(chan delegate.TaskEvent, 1)
	h = newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "parent" {
			return gated(g, "the answer")(ctx, in)
		}
		parentCalls.Add(1)
		notices.Add(int32(noticeReminders(in)))
		switch len(in.Messages) {
		case 1:
			return costed(taskCall("find the parser"), "1"), nil
		case 3:
			first(g, ended)
			return costed(toolCall("status-1", delegate.StatusToolName,
				`{"task_id":"`+sessionTaskID(s)+`","wait_seconds":30}`), "1"), nil
		}
		return costed(text("parent read the status"), "1"), nil
	}, delegate.Config{}, nil)
	s = sessionTasks(t, h)
	// Forward TaskEnded to the model script, and keep it for the checks.
	cfg := h.cfg
	onTask := cfg.OnTask
	cfg.OnTask = func(e delegate.TaskEvent) {
		onTask(e)
		if e.Kind == delegate.TaskEnded {
			select {
			case ended <- e:
			default:
			}
		}
	}
	h.cfg = cfg
	h.newTasks(t)
	s.Runner = h.runner
	out, err := s.Send(t.Context(), "go")
	if err != nil || out.Reply != "parent read the status" {
		t.Fatalf("answer=%q err=%v", out.Reply, err)
	}
	if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 1 || st.Queued != 0 {
		t.Fatalf("state=%+v err=%v: the result started another turn", st, err)
	}
	if n := notices.Load(); n != 0 || parentCalls.Load() != 3 {
		t.Fatalf("notices=%d parent calls=%d", n, parentCalls.Load())
	}
	e := h.next(t, delegate.TaskEnded)
	if name == "read waits, then the child ends" && !e.AlreadyDelivered {
		t.Fatal("TaskEnded does not say the read delivered the result")
	}
	turns := sessionTurns(t, s)
	if !delegate.DeliveredNotices(turns)[delegate.NoticeID(e.Task.TurnID)] {
		t.Fatal("the record does not show the read as delivery")
	}
	if got := statusDelivered(turns); len(got) != 1 || got[0] != e.Task.TurnID {
		t.Fatalf("status read delivered %q, want %q", got, e.Task.TurnID)
	}

	// A reopened host finds the read in the record and sends nothing, even
	// though it cuts replies shorter, so the result's text has changed.
	h.cfg.MaxReply = 4
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner, turns); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
	if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 1 || st.Queued != 0 {
		t.Fatalf("after reopen state=%+v err=%v", st, err)
	}
}

// A read that shows a running task, or the list of tasks without replies,
// delivers nothing: the notice still follows when the turn ends.
func TestStatusReadWithoutTheFinalResultDeliversNothing(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "the answer")), delegate.Config{}, nil)
	h.notify.Store(true)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	got, err := h.call(t, delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || !strings.Contains(got, "running") {
		t.Fatalf("status=%q err=%v", got, err)
	}
	// The list waits for the end but carries no reply.
	go g.open()
	got, err = h.call(t, delegate.StatusToolName, `{"wait_seconds":30}`)
	if err != nil || strings.Contains(got, "the answer") {
		t.Fatalf("list=%q err=%v", got, err)
	}
	if ended := h.next(t, delegate.TaskEnded); ended.AlreadyDelivered {
		t.Fatal("a read without the result counted as delivery")
	}
	parent, _ := h.rec.Load(t.Context(), parentTurn)
	left, err := h.inbox.Leftovers(parent)
	if err != nil || len(left) != 1 || !isNotice(left[0]) {
		t.Fatalf("leftovers=%+v err=%v", left, err)
	}
}

// statusDelivered returns the child turns that the recorded task_status
// calls in turns name as delivered, in order.
func statusDelivered(turns []dashi.Turn) []string {
	var out []string
	for _, turn := range turns {
		for _, step := range turn.Steps {
			f := step.Tool
			if step.Kind != dashi.StepToolFinished || f == nil ||
				f.Call.Name != delegate.StatusToolName || f.Outcome == nil ||
				len(f.Outcome.Data) == 0 {
				continue
			}
			var d struct {
				TurnID string `json:"delivered_turn_id"`
			}
			if json.Unmarshal(f.Outcome.Data, &d) == nil {
				out = append(out, d.TurnID)
			}
		}
	}
	return out
}

// Two tasks with the same reply are two results. A task_status read of one
// delivers that one only: the other still gets its notice, and a reopened
// host sends neither again.
func TestStatusReadDeliversOnlyTheTaskItRead(t *testing.T) {
	first, second := newGate(), newGate()
	var parentCalls atomic.Int32
	var mu sync.Mutex
	var notices []string // the sender of each task-ended reminder
	var s *session.Session
	h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "parent" {
			g := first
			if in.Messages[0].Text() == "second" {
				g = second
			}
			return gated(g, "the same answer")(ctx, in)
		}
		mu.Lock()
		for _, m := range in.Messages {
			for _, c := range m.Content {
				if c.Reminder != nil && c.Reminder.Name == "task-ended" {
					notices = append(notices, c.Reminder.From)
				}
			}
		}
		mu.Unlock()
		switch parentCalls.Add(1) {
		case 1:
			r := taskCall("first")
			b := toolCall("call-2", delegate.ToolName,
				`{"subagent":"scout","prompt":"second","description":"look"}`)
			r.Message.Content = append(r.Message.Content, b.Message.Content...)
			return costed(r, "1"), nil
		case 2:
			go func() { time.Sleep(50 * time.Millisecond); first.open() }()
			return costed(toolCall("status-1", delegate.StatusToolName,
				`{"task_id":"`+sessionTaskID(s)+`","wait_seconds":30}`), "1"), nil
		case 3:
			second.open()
			return costed(text("parent read the first"), "1"), nil
		}
		return costed(text("parent read the second"), "1"), nil
	}, delegate.Config{}, nil)
	s = sessionTasks(t, h)
	if _, err := s.Send(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
		t.Fatal(err)
	}
	parent := session.TurnID(s.ID, 1)
	firstTurn := session.TurnID(delegate.ChildID(parent, callID), 1)
	secondTurn := session.TurnID(delegate.ChildID(parent, "call-2"), 1)
	secondTask := delegate.TaskID(delegate.ChildID(parent, "call-2"))
	mu.Lock()
	got := notices
	mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], secondTask) {
		t.Fatalf("notices from %q, want one from %s", got, secondTask)
	}
	turns := sessionTurns(t, s)
	if read := statusDelivered(turns); len(read) != 1 || read[0] != firstTurn {
		t.Fatalf("status read delivered %q, want %q", read, firstTurn)
	}
	delivered := delegate.DeliveredNotices(turns)
	if !delivered[delegate.NoticeID(firstTurn)] ||
		!delivered[delegate.NoticeID(secondTurn)] {
		t.Fatalf("delivered=%v", delivered)
	}
	for range 2 {
		e := h.next(t, delegate.TaskEnded)
		if e.AlreadyDelivered != (e.Task.TurnID == firstTurn) {
			t.Fatalf("%s: AlreadyDelivered=%v", e.Task.TurnID, e.AlreadyDelivered)
		}
	}

	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner, turns); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
}

// A child is told its own task ID, so it can name itself in its reply.
func TestChildKnowsItsTaskID(t *testing.T) {
	h := newHarness(t, script("found"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	in := h.client.last("child")
	if !strings.Contains(in.System, "Your task ID is "+taskID()+".") {
		t.Fatalf("child system=%q", in.System)
	}
	if got := h.view(t, childTurnID()).Agent.Instructions; !strings.Contains(got, taskID()) {
		t.Fatalf("recorded instructions=%q", got)
	}
}

// cancelAfterStatus wraps the task_status tool of ts: after the real call
// returns, it cancels the turn and holds its result until the runner has
// stopped, so the result is never recorded.
type cancelAfterStatus struct {
	ts     tool.Toolset
	cancel context.CancelFunc
}

func (c cancelAfterStatus) Tools(ctx context.Context) ([]tool.Tool, error) {
	list, err := c.ts.Tools(ctx)
	for i, tl := range list {
		if tl.Definition.Name != delegate.StatusToolName {
			continue
		}
		inner := tl.Executor
		list[i].Executor = executor(func(ctx context.Context, call tool.Call) (tool.Outcome, error) {
			out, err := inner.Execute(ctx, call)
			c.cancel()
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			return out, err
		})
	}
	return list, err
}

// A read that took the notice's place but whose result the parent never
// recorded, because the turn was canceled first, does not lose the result:
// the notice reaches the agent on the next turn, without a reopen.
func TestUnrecordedStatusReadGivesTheResultBack(t *testing.T) {
	g := newGate()
	var mu sync.Mutex
	var noticeCalls []int // parent call numbers that carried a notice
	var parentCalls atomic.Int32
	var s *session.Session
	h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "parent" {
			return gated(g, "the answer")(ctx, in)
		}
		n := parentCalls.Add(1)
		if k := noticeReminders(in); k > 0 {
			mu.Lock()
			for range k {
				noticeCalls = append(noticeCalls, int(n))
			}
			mu.Unlock()
		}
		switch n {
		case 1:
			return costed(taskCall("find the parser"), "1"), nil
		case 2:
			go func() { time.Sleep(50 * time.Millisecond); g.open() }()
			return costed(toolCall("status-1", delegate.StatusToolName,
				`{"task_id":"`+sessionTaskID(s)+`","wait_seconds":30}`), "1"), nil
		}
		return costed(text("parent done"), "1"), nil
	}, delegate.Config{}, nil)
	s = sessionTasks(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Runner.Tools = tool.Join(h.base, cancelAfterStatus{h.tasks.Tools(), cancel})
	out, _ := s.Send(ctx, "go")
	if out.Stop.Kind != dashi.StopInterrupted && out.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("first turn stop=%+v", out.Stop)
	}
	turns := sessionTurns(t, s)
	if len(statusDelivered(turns)) != 0 {
		t.Fatal("the canceled read was recorded")
	}
	// The claim gives the result back: the notice goes out as usual.
	if e := h.next(t, delegate.TaskEnded); e.AlreadyDelivered {
		t.Fatal("an unrecorded read counted as delivery")
	}
	s.Runner.Tools = tool.Join(h.base, h.tasks.Tools())
	if _, err := s.Send(t.Context(), "next"); err != nil {
		t.Fatal(err)
	}
	if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := noticeCalls
	mu.Unlock()
	if len(got) != 1 || got[0] < 3 {
		t.Fatalf("notice reached parent calls %v, want one after the canceled turn", got)
	}
	turns = sessionTurns(t, s)
	if !delegate.DeliveredNotices(turns)[delegate.NoticeID(sessionChildTurn(s))] {
		t.Fatal("the record does not show the notice delivered")
	}
}
