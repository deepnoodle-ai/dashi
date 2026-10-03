package delegate_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/session"
)

// A task runs in a child session; its first turn is "<child>-1".
func TestTaskRunsAsTurnOneOfAChildSession(t *testing.T) {
	h := newHarness(t, script("found"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	v := ended.Task
	if v.SessionID != childID() || v.TurnID != childTurnID() || v.Turns != 1 {
		t.Fatalf("view session=%q turn=%q turns=%d", v.SessionID, v.TurnID, v.Turns)
	}
	if got := h.view(t, childTurnID()).Reply; got != "found" {
		t.Fatalf("child turn 1 answer=%q", got)
	}
	if _, err := h.rec.Load(t.Context(), childID()); err == nil {
		t.Fatal("a turn is recorded under the session ID itself")
	}
}

// A notice is a task-ended reminder from the task, with origin
// ContextAgent, whose sender stays valid whatever the subagent's name.
func TestNoticeIsAReminderFromTheTask(t *testing.T) {
	sub := scout()
	sub.Name = `odd "name" <` + strings.Repeat("x", 80)
	cfg := delegate.Config{Subagents: []delegate.Subagent{sub}}
	h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if role(in) == "parent" && len(in.Messages) == 1 {
			r := taskCall("find the parser")
			r.Message.Content[0].ToolCall.Args = []byte(
				`{"subagent":` + quote(sub.Name) + `,"prompt":"p","description":"d"}`)
			return r, nil
		}
		return script("found")(ctx, in)
	}, cfg, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	item := h.next(t, delegate.TaskEnded).Task.Notice().Item()
	if item.ID != delegate.NoticeID(childTurnID()) || item.Origin != dashi.ContextAgent {
		t.Fatalf("notice id=%q origin=%q", item.ID, item.Origin)
	}
	if len(item.Message.Content) != 1 || item.Message.Content[0].Reminder == nil {
		t.Fatalf("notice message=%+v", item.Message)
	}
	r := *item.Message.Content[0].Reminder
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Name != "task-ended" || r.Tier != llm.ReminderContextual ||
		!strings.HasPrefix(r.From, "task "+taskID()+" (odd name ") || len(r.From) > 64 {
		t.Fatalf("reminder name=%q tier=%q from=%q", r.Name, r.Tier, r.From)
	}
	if !strings.HasPrefix(r.Content, "[task "+taskID()+" completed]") {
		t.Fatalf("content=%q", r.Content)
	}
	// A session takes it as input.
	s := &session.Session{Runner: h.runner, Agent: h.agent}
	if _, err := s.EnqueueItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// With Config.Session, Tasks delivers each notice itself: into the running
// turn, or, when no turn runs, as a queue turn.
func TestConfigSessionDeliversNotices(t *testing.T) {
	t.Run("into the running turn", func(t *testing.T) {
		g, parentHeld := newGate(), newGate()
		handle := scripted(gated(g, "it is in config/load.go"))
		h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
			if role(in) == "parent" && len(in.Messages) == 3 {
				if err := parentHeld.wait(ctx); err != nil {
					return llm.Result{}, err
				}
			}
			return handle(ctx, in)
		}, delegate.Config{}, nil)
		s := sessionTasks(t, h)
		done := make(chan dashi.RunResult, 1)
		go func() {
			out, _ := s.Send(t.Context(), "go")
			done <- out
		}()
		parentHeld.waitStarted(t)
		g.open()
		h.next(t, delegate.TaskEnded)
		parentHeld.open()
		out := <-done
		if out.Reply != "parent read the notice" {
			t.Fatalf("answer=%q: the turn did not read the notice", out.Reply)
		}
		if n := countItems(out.Turn, delegate.NoticeID(sessionChildTurn(s))); n != 1 {
			t.Fatalf("notice recorded %d times, want 1", n)
		}
		if st, err := s.Open(t.Context()); err != nil || st.Turns != 1 || st.Queued != 0 {
			t.Fatalf("state=%+v err=%v", st, err)
		}
	})
	t.Run("as a queue turn", func(t *testing.T) {
		g := newGate()
		h := newHarness(t, scripted(gated(g, "found")), delegate.Config{}, nil)
		s := sessionTasks(t, h)
		if out, err := s.Send(t.Context(), "go"); err != nil || out.Reply != "parent done" {
			t.Fatalf("answer=%q err=%v", out.Reply, err)
		}
		g.open()
		out, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks)
		if err != nil {
			t.Fatal(err)
		}
		if out.Turn.ID != session.TurnID("conv", 2) || out.Origin != dashi.StartScheduled ||
			out.Reply != "parent read the notice" {
			t.Fatalf("turn=%s origin=%q answer=%q", out.Turn.ID, out.Origin, out.Reply)
		}
		key := out.Turn.Steps[0].CommandID
		if key != `queue:["`+delegate.NoticeID(sessionChildTurn(s))+`"]` {
			t.Fatalf("queue key=%q", key)
		}
		h.next(t, delegate.TaskEnded)
		// The record holds the delivery, so a reopened host sends nothing.
		var turns []dashi.Turn
		for turn, err := range s.Turns(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			turns = append(turns, turn)
		}
		h.newTasks(t)
		if err := h.tasks.Recover(t.Context(), h.runner, turns); err != nil {
			t.Fatal(err)
		}
		h.none(t, delegate.TaskEnded)
	})
}

// A Config.Session that refuses every steer gets the notice on its queue;
// any other error is logged as it is, not as a refusal.
func TestConfigSessionFallsBackToTheQueueOnRefusal(t *testing.T) {
	// delivered counts the notice across the turns of session "conv": as a
	// steer, or in the command ID of a queue turn.
	delivered := func(t *testing.T, s *session.Session) int {
		t.Helper()
		id := delegate.NoticeID(sessionChildTurn(s))
		n := 0
		for turn, err := range s.Turns(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			n += countItems(turn, id)
			if strings.Contains(turn.Steps[0].CommandID, `"`+id+`"`) {
				n++
			}
		}
		return n
	}
	t.Run("host steering source", func(t *testing.T) {
		g := newGate()
		h := newHarness(t, scripted(gated(g, "found")), delegate.Config{}, nil)
		s := sessionTasks(t, h)
		s.Steering = hostSteering{}
		if _, err := s.Send(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		g.open()
		h.next(t, delegate.TaskEnded)
		if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
			t.Fatal(err)
		}
		if n := delivered(t, s); n != 1 {
			t.Fatalf("notice recorded %d times, want 1", n)
		}
	})
	t.Run("compaction turn", func(t *testing.T) {
		g, summary := newGate(), newGate()
		child := scripted(gated(g, "found"))
		h := newHarness(t, func(ctx context.Context, in llm.Input) (llm.Result, error) {
			if strings.Contains(inputText(in), "summarize now") {
				if err := summary.wait(ctx); err != nil {
					return llm.Result{}, err
				}
				return text("<summary>a task runs</summary>"), nil
			}
			return child(ctx, in)
		}, delegate.Config{}, nil)
		c := &onceCompactor{}
		s := sessionTasks(t, h)
		s.Compactor = c
		if _, err := s.Send(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		c.due.Store(true)
		done := make(chan error, 1)
		go func() {
			_, err := s.Send(t.Context(), "next")
			done <- err
		}()
		summary.waitStarted(t)
		g.open()
		h.next(t, delegate.TaskEnded)
		summary.open()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks); err != nil {
			t.Fatal(err)
		}
		if n := delivered(t, s); n != 1 {
			t.Fatalf("notice recorded %d times, want 1", n)
		}
	})
	t.Run("other error", func(t *testing.T) {
		var logs bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(old) })
		g := newGate()
		h := newHarness(t, scripted(gated(g, "found")), delegate.Config{}, nil)
		s := sessionTasks(t, h)
		if _, err := s.Send(t.Context(), "go"); err != nil {
			t.Fatal(err)
		}
		// An item that already holds the notice's ID makes the steer fail
		// for a reason of its own.
		if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
			ID:      delegate.NoticeID(sessionChildTurn(s)),
			Origin:  dashi.ContextUser,
			Message: llm.UserMessage(llm.Text("taken")),
		}); err != nil {
			t.Fatal(err)
		}
		g.open()
		h.next(t, delegate.TaskEnded)
		got := logs.String()
		if !strings.Contains(got, "could not deliver a notice") ||
			!strings.Contains(got, "already queued") {
			t.Fatalf("logs=%q", got)
		}
	})
}

// hostSteering is a steering source that the host supplies, so the session
// refuses SteerItem.
type hostSteering struct{}

func (hostSteering) Steers(context.Context, string) ([]dashi.Steer, error) {
	return nil, nil
}

func (hostSteering) Delivered(context.Context, string, []string) error {
	return nil
}

// onceCompactor asks for one compaction turn once due is set. The turn
// replaces the history with its summary.
type onceCompactor struct {
	due atomic.Bool
}

func (c *onceCompactor) Needed([]llm.Message, llm.Usage) bool {
	return c.due.Load()
}

func (c *onceCompactor) Start(context.Context, []llm.Message) (dashi.Start, error) {
	c.due.Store(false)
	return dashi.Start{
		Input:      []llm.Message{llm.UserMessage(llm.Text("summarize now"))},
		Compaction: &dashi.CompactionFact{Frame: "Summary:"},
	}, nil
}

// sessionTasks gives the harness a parent session "conv" that its Tasks
// delivers to.
func sessionTasks(t *testing.T, h *harness) *session.Session {
	t.Helper()
	s := &session.Session{ID: "conv", New: true, Agent: h.agent}
	h.cfg.Session = s
	h.newTasks(t)
	s.Runner = h.runner
	return s
}

// sessionChildTurn is the first child turn of the task that turn 1 of
// session "conv" started.
func sessionChildTurn(s *session.Session) string {
	return session.TurnID(delegate.ChildID(session.TurnID(s.ID, 1), callID), 1)
}

// A later child turn counts under the parent turn whose call started it, and
// Recover resumes it when interrupted and reports each ended turn once.
func TestLaterChildTurnsAcrossRecover(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "now list the tests") {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return answers("done")(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	sender := interruptTurnTwo(t, h, g)
	g.open()

	turn2 := session.TurnID(childID(), 2)
	h.newTasks(t)
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	// Turn 1's end and turn 2's start come from different goroutines.
	events := map[string]int{}
	for range 3 {
		select {
		case e := <-h.taskEvents:
			events[string(e.Kind)+" "+e.Task.TurnID]++
		case <-time.After(5 * time.Second):
			t.Fatalf("events=%v", events)
		}
	}
	if events["started "+turn2] != 1 || events["ended "+childTurnID()] != 1 ||
		events["ended "+turn2] != 1 {
		t.Fatalf("events=%v", events)
	}
	h.none(t, delegate.TaskEnded)
	if v := h.view(t, turn2); v.Stop.Kind != dashi.StopCompleted ||
		v.Resumes != 1 || v.Parent == nil || *v.Parent != sender {
		t.Fatalf("turn 2 stop=%+v interruptions=%d parent=%+v",
			v.Stop, v.Resumes, v.Parent)
	}
	if _, err := h.rec.Load(t.Context(), session.TurnID(childID(), 3)); err == nil {
		t.Fatal("Recover started a third turn")
	}

	usd := money.MustParseAmount
	v := h.tasks.List()[0]
	if v.Turns != 2 || v.TurnID != turn2 || v.Reply != "done" ||
		v.Spend.Cost["USD"] != usd("0.5") {
		t.Fatalf("view turns=%d turn=%q answer=%q spend=%v",
			v.Turns, v.TurnID, v.Reply, v.Spend.Cost)
	}
	// Turn 2 counts under parent turn 7, not under the turn that started
	// the task.
	report, err := delegate.Report(t.Context(), h.rec, parent.TurnView)
	if err != nil {
		t.Fatal(err)
	}
	if report.Children.Cost["USD"] != usd("0.25") {
		t.Fatalf("report=%+v", report)
	}

	// Once the parent's record holds both notices, a reopen reports none.
	if err := h.recoverTasks(t); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
	conv := &session.Session{New: true, Runner: h.runner, Agent: h.agent}
	for _, turn := range []string{childTurnID(), turn2} {
		v.TurnID = turn
		if _, err := conv.EnqueueItem(t.Context(), v.Notice().Item()); err != nil {
			t.Fatal(err)
		}
	}
	queued, ran, err := conv.RunQueue(t.Context())
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	first, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{first, queued.Turn}); err != nil {
		t.Fatal(err)
	}
	h.none(t, delegate.TaskEnded)
}

// interruptTurnTwo starts turn 2 of the child from a call of another parent
// turn, as task_send will, and interrupts it, as a crash does, once its
// model call waits on g. It returns the call that started the turn.
func interruptTurnTwo(t *testing.T, h *harness, g *gate) dashi.CallRef {
	t.Helper()
	sender := dashi.CallRef{TurnID: "parent-turn-7", CallID: "send-1"}
	s := &session.Session{
		ID:        childID(),
		Exclusive: true,
		Runner: dashi.Runner{
			Client:        h.client,
			Recorder:      h.rec,
			OnContextDone: dashi.ContextInterrupt,
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.RunResult, 1)
	go func() {
		out, _ := s.Run(ctx, dashi.Start{
			CommandID: "msg:parent-turn-7/send-1",
			Origin:    dashi.StartScheduled,
			Parent:    &sender,
			Input:     []llm.Message{llm.UserMessage(llm.Text("now list the tests"))},
		})
		done <- out
	}()
	g.waitStarted(t)
	cancel()
	if out := <-done; out.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("turn 2 stop=%+v", out.Stop)
	}
	return sender
}

// Recover marks every delivered turn before it resumes any turn, so a
// resumed turn that ends at once never reports an earlier turn again.
func TestRecoverMarksDeliveredTurnsBeforeResuming(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "now list the tests") {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return answers("done")(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	notice := h.next(t, delegate.TaskEnded).Task.Notice().Item()
	interruptTurnTwo(t, h, g)
	g.open()

	// The parent's record holds turn 1's notice; turn 2 is unfinished.
	conv := &session.Session{New: true, Runner: h.runner, Agent: h.agent}
	if _, err := conv.EnqueueItem(t.Context(), notice); err != nil {
		t.Fatal(err)
	}
	queued, ran, err := conv.RunQueue(t.Context())
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	first, err := h.rec.Load(t.Context(), parentTurn)
	if err != nil {
		t.Fatal(err)
	}
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{first, queued.Turn}); err != nil {
		t.Fatal(err)
	}
	turn2 := session.TurnID(childID(), 2)
	if e := h.next(t, delegate.TaskEnded); e.Task.TurnID != turn2 {
		t.Fatalf("ended turn %q, want %q", e.Task.TurnID, turn2)
	}
	h.none(t, delegate.TaskEnded)
}
