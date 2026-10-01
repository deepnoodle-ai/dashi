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

// readOrder is when the child ends relative to the parent's task_status
// call, which waits up to 30 seconds.
type readOrder struct {
	name string
	runs int
	// first runs in the model call that returns the task_status call.
	first func(g *gate, ended <-chan delegate.TaskEvent)
}

var readOrders = []readOrder{
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

// readScript is a parent that starts a task, reads its result with a
// waiting task_status call, and answers. It counts the parent's model calls
// and the notices they carry.
func readScript(
	g *gate,
	o readOrder,
	s **session.Session,
	ended <-chan delegate.TaskEvent,
	calls, notices *atomic.Int32,
) modelFunc {
	return func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) != "parent" {
			return gated(g, "the answer")(ctx, in)
		}
		calls.Add(1)
		notices.Add(int32(noticeReminders(in)))
		switch len(in.Messages) {
		case 1:
			return costed(taskCall("find the parser"), "1"), nil
		case 3:
			o.first(g, ended)
			return costed(toolCall("status-1", delegate.StatusToolName,
				`{"task_id":"`+sessionTaskID(*s)+`","wait_seconds":30}`), "1"), nil
		}
		return costed(text("parent read the status"), "1"), nil
	}
}

// forwardEnded passes each TaskEnded event of h to ended as well.
func forwardEnded(t *testing.T, h *harness, ended chan delegate.TaskEvent) {
	onTask := h.cfg.OnTask
	h.cfg.OnTask = func(e delegate.TaskEvent) {
		onTask(e)
		if e.Kind == delegate.TaskEnded {
			select {
			case ended <- e:
			default:
			}
		}
	}
	h.newTasks(t)
}

// The user's case: the parent reads a task's final result with task_status
// in turn 1. A host that holds notices until the turn ends checks the
// record first, so no notice is delivered and no other turn runs, now or
// after a reopen that cuts replies shorter. Each order of the child's end
// and the read holds.
func TestStatusReadOfTheFinalResultStartsNoTurn(t *testing.T) {
	for _, o := range readOrders {
		for i := range o.runs {
			t.Run(fmt.Sprintf("%s/%d", o.name, i), func(t *testing.T) {
				g := newGate()
				var calls, notices atomic.Int32
				var s *session.Session
				ended := make(chan delegate.TaskEvent, 1)
				h := newHarness(t, readScript(g, o, &s, ended, &calls, &notices),
					delegate.Config{}, nil)
				forwardEnded(t, h, ended)
				// The host queues each notice, which waits for the turn to end,
				// and the session checks the record before a queue turn.
				s = &session.Session{ID: "conv", New: true, Agent: h.agent, Runner: h.runner}
				s.Delivered = delegate.DeliveredNotices
				h.session.Store(s)
				checkOneTurn(t, h, s, &calls, &notices, 0)
			})
		}
	}
}

// With Config.Session, a notice that reaches the running turn is not
// checked, so the model may see the result twice in turn 1, but no other
// turn runs for it.
func TestStatusReadWithConfigSessionStartsNoTurn(t *testing.T) {
	for _, o := range readOrders {
		for i := range o.runs {
			t.Run(fmt.Sprintf("%s/%d", o.name, i), func(t *testing.T) {
				g := newGate()
				var calls, notices atomic.Int32
				var s *session.Session
				ended := make(chan delegate.TaskEvent, 1)
				h := newHarness(t, readScript(g, o, &s, ended, &calls, &notices),
					delegate.Config{}, nil)
				s = sessionTasks(t, h)
				forwardEnded(t, h, ended)
				s.Runner = h.runner
				checkOneTurn(t, h, s, &calls, &notices, 1)
			})
		}
	}
}

// checkOneTurn sends turn 1 and checks that the read result started no
// other turn, carried at most maxNotices notices, and is recorded as
// delivered, so a reopened host sends nothing either.
func checkOneTurn(
	t *testing.T,
	h *harness,
	s *session.Session,
	calls, notices *atomic.Int32,
	maxNotices int32,
) {
	t.Helper()
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
	if n := notices.Load(); n > maxNotices || calls.Load() != 3 {
		t.Fatalf("notices=%d parent calls=%d", n, calls.Load())
	}
	e := h.next(t, delegate.TaskEnded)
	turns := sessionTurns(t, s)
	if !delegate.DeliveredNotices(turns)[delegate.NoticeID(e.Task.TurnID)] {
		t.Fatal("the record does not show the read as delivery")
	}
	if got := statusDelivered(turns); len(got) != 1 || got[0] != e.Task.TurnID {
		t.Fatalf("status read delivered %q, want %q", got, e.Task.TurnID)
	}

	// A reopened host finds the read in the record and sends nothing, even
	// though it cuts replies shorter, so the result's text has changed.
	h.session.Store(nil)
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

// Only a read that returns a turn's final result names it. A read of a
// running task, the list of tasks, and a read of a stopped child whose
// notice waits for the host name nothing, and the notice still follows.
func TestStatusReadWithoutTheFinalResultDeliversNothing(t *testing.T) {
	g := newGate()
	h := newHarness(t, scripted(gated(g, "the answer")), delegate.Config{}, nil)
	h.notify.Store(true)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out, err := h.exec(t.Context(), delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || len(out.Data) != 0 {
		t.Fatalf("running read data=%s err=%v", out.Data, err)
	}
	// The list waits for the end but carries no reply.
	go g.open()
	out, err = h.exec(t.Context(), delegate.StatusToolName, `{"wait_seconds":30}`)
	if err != nil || len(out.Data) != 0 {
		t.Fatalf("list data=%s err=%v", out.Data, err)
	}
	h.next(t, delegate.TaskEnded)
	parent, _ := h.rec.Load(t.Context(), parentTurn)
	left, err := h.inbox.Leftovers(parent)
	if err != nil || len(left) != 1 || !isNotice(left[0]) {
		t.Fatalf("leftovers=%+v err=%v", left, err)
	}
	// The final result names its turn.
	out, err = h.exec(t.Context(), delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || string(out.Data) != `{"delivered_turn_id":"`+childTurnID()+`"}` {
		t.Fatalf("final read data=%s err=%v", out.Data, err)
	}

	// A stopped child whose notice is deferred has no final result yet.
	w := waitingChild(t, nil)
	out, err = w.exec(t.Context(), delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || len(out.Data) != 0 {
		t.Fatalf("deferred read data=%s err=%v", out.Data, err)
	}
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
	h.agent.Limits.ToolConcurrency = 2
	s = &session.Session{ID: "conv", New: true, Agent: h.agent, Runner: h.runner}
	s.Delivered = delegate.DeliveredNotices
	h.session.Store(s)
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
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 2 || st.Queued != 0 {
		t.Fatalf("state=%+v err=%v", st, err)
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
	h.next(t, delegate.TaskEnded)
	h.next(t, delegate.TaskEnded)

	h.session.Store(nil)
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

// A read whose result the parent never recorded, because the turn was
// canceled first, is not delivery: the notice reaches the agent on the next
// turn, without a reopen.
func TestUnrecordedStatusReadLeavesTheNotice(t *testing.T) {
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
	h.next(t, delegate.TaskEnded)
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

// A read counts as delivery only when the model saw the task_status result
// the tool returned. When an AfterTool hook replaces it or the result limit
// shortens it, the record drops the marker, so the notice still reaches the
// parent with the whole reply, in one more turn. A hook that leaves the
// result as it is changes nothing: no other turn runs.
func TestStatusReadChangedBeforeTheModelSawItLeavesTheNotice(t *testing.T) {
	reply := "the answer " + strings.Repeat("x", 3000) + " end of reply"
	onStatus := func(f func(llm.ToolResult) llm.ToolResult) dashi.Hooks {
		return dashi.Hooks{AfterTool: []dashi.ResultHook{{Name: "status", Func: func(
			_ context.Context,
			p dashi.ResultPlan,
		) (dashi.ResultDecision, error) {
			if p.Call.Name != delegate.StatusToolName {
				return dashi.ResultDecision{}, nil
			}
			r := f(p.Result)
			return dashi.ResultDecision{Replace: &r}, nil
		}}}}
	}
	tests := []struct {
		name      string
		hooks     dashi.Hooks
		limit     int
		delivered bool // the read counts as delivery
	}{
		{"replaced by a hook", onStatus(func(llm.ToolResult) llm.ToolResult {
			return llm.ToolResult{Output: tool.TextOutput("[status hidden]")}
		}), 0, false},
		{"shortened by the result limit", dashi.Hooks{}, 1024, false},
		{"unchanged by a hook", onStatus(func(r llm.ToolResult) llm.ToolResult { return r }), 0, true},
		// Display never reaches the model, so the read still counts.
		{"display changed by a hook", onStatus(func(r llm.ToolResult) llm.ToolResult {
			r.Output.Display = "task done"
			return r
		}), 0, true},
	}
	order := readOrders[1] // the child ends, then the read
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := newGate()
			var calls, notices atomic.Int32
			var mu sync.Mutex
			var noticeText []string
			var s *session.Session
			ended := make(chan delegate.TaskEvent, 1)
			parent := readScript(g, order, &s, ended, &calls, &notices)
			h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
				if role(in) != "parent" {
					return gated(g, reply)(ctx, in)
				}
				for _, m := range in.Messages {
					for _, c := range m.Content {
						if c.Reminder != nil && c.Reminder.Name == "task-ended" {
							mu.Lock()
							noticeText = append(noticeText, c.Reminder.Content)
							mu.Unlock()
						}
					}
				}
				return parent(ctx, in)
			}, delegate.Config{}, nil)
			forwardEnded(t, h, ended)
			agent := h.agent
			agent.Limits.ToolResultBytes = test.limit
			runner := h.runner
			runner.Hooks = test.hooks
			s = &session.Session{ID: "conv", New: true, Agent: agent, Runner: runner}
			s.Delivered = delegate.DeliveredNotices
			h.session.Store(s)

			if _, err := s.Send(t.Context(), "go"); err != nil {
				t.Fatal(err)
			}
			if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
				t.Fatal(err)
			}
			e := h.next(t, delegate.TaskEnded)
			turns := sessionTurns(t, s)
			st, err := s.Open(t.Context())
			if err != nil || st.Queued != 0 {
				t.Fatalf("state=%+v err=%v", st, err)
			}
			warned := false
			for _, turn := range turns {
				for _, step := range turn.Steps {
					for _, w := range step.Warnings {
						warned = warned || w.Code == dashi.WarningToolDataDropped
					}
				}
			}
			if test.delivered {
				if st.Turns != 1 || notices.Load() != 0 || warned ||
					len(statusDelivered(turns)) != 1 {
					t.Fatalf("turns=%d notices=%d marker=%v",
						st.Turns, notices.Load(), statusDelivered(turns))
				}
				return
			}
			if st.Turns != 2 || notices.Load() != 1 || len(statusDelivered(turns)) != 0 {
				t.Fatalf("turns=%d notices=%d marker=%v: the result was lost",
					st.Turns, notices.Load(), statusDelivered(turns))
			}
			mu.Lock()
			got := strings.Join(noticeText, "")
			mu.Unlock()
			if !strings.Contains(got, reply) {
				t.Fatalf("the notice lacks the whole reply: %.80q", got)
			}
			if !delegate.DeliveredNotices(turns)[delegate.NoticeID(e.Task.TurnID)] {
				t.Fatal("the record does not show the notice delivered")
			}
		})
	}
}
