package delegate_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
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
	if name == "read waits, then the child ends" && !e.Delivered {
		t.Fatal("TaskEnded does not say the read delivered the result")
	}
	turns := sessionTurns(t, s)
	if !e.Task.Notice().DeliveredIn(turns) {
		t.Fatal("the record does not show the read as delivery")
	}

	// A reopened host finds the read in the record and sends nothing.
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
	if ended := h.next(t, delegate.TaskEnded); ended.Delivered {
		t.Fatal("a read without the result counted as delivery")
	}
	parent, _ := h.rec.Load(t.Context(), parentTurn)
	left, err := h.inbox.Leftovers(parent)
	if err != nil || len(left) != 1 || !isNotice(left[0]) {
		t.Fatalf("leftovers=%+v err=%v", left, err)
	}
}

// The final read carries the notice text, so a reopened host can match it.
func TestFinalStatusCarriesTheNoticeText(t *testing.T) {
	h := newHarness(t, script("the answer"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	got, err := h.call(t, delegate.StatusToolName, `{"task_id":"`+taskID()+`"}`)
	if err != nil || !strings.HasSuffix(got, "\n"+ended.Task.Notice().Text()) {
		t.Fatalf("status=%q err=%v", got, err)
	}
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
