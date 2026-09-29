package delegate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
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
	if got := h.view(t, childTurnID()).Answer; got != "found" {
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
	s := &dashi.Session{Runner: h.runner, Agent: h.agent}
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
		done := make(chan dashi.Outcome, 1)
		go func() {
			out, _ := s.Send(t.Context(), "go")
			done <- out
		}()
		parentHeld.waitStarted(t)
		g.open()
		h.next(t, delegate.TaskEnded)
		parentHeld.open()
		out := <-done
		if out.Answer != "parent read the notice" {
			t.Fatalf("answer=%q: the turn did not read the notice", out.Answer)
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
		if out, err := s.Send(t.Context(), "go"); err != nil || out.Answer != "parent done" {
			t.Fatalf("answer=%q err=%v", out.Answer, err)
		}
		g.open()
		out, err := delegate.RunUntilQuiet(t.Context(), s, h.tasks)
		if err != nil {
			t.Fatal(err)
		}
		if out.Turn.ID != dashi.TurnID("conv", 2) || out.Origin != dashi.StartScheduled ||
			out.Answer != "parent read the notice" {
			t.Fatalf("turn=%s origin=%q answer=%q", out.Turn.ID, out.Origin, out.Answer)
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

// sessionTasks gives the harness a parent session "conv" that its Tasks
// delivers to.
func sessionTasks(t *testing.T, h *harness) *dashi.Session {
	t.Helper()
	s := &dashi.Session{ID: "conv", New: true, Agent: h.agent}
	h.cfg.Session = s
	h.newTasks(t)
	s.Runner = h.runner
	return s
}

// sessionChildTurn is the first child turn of the task that turn 1 of
// session "conv" started.
func sessionChildTurn(s *dashi.Session) string {
	return dashi.TurnID(delegate.ChildID(dashi.TurnID(s.ID, 1), callID), 1)
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

	// Turn 2 starts from a call of another parent turn, as task_send will,
	// and is interrupted, as by a crash.
	sender := dashi.CallRef{TurnID: "parent-turn-7", CallID: "send-1"}
	s := &dashi.Session{
		ID:        childID(),
		Exclusive: true,
		Runner: dashi.Runner{
			Client:        h.client,
			Recorder:      h.rec,
			OnContextDone: dashi.ContextInterrupt,
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan dashi.Outcome, 1)
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
	g.open()

	turn2 := dashi.TurnID(childID(), 2)
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
		v.Interruptions != 1 || v.Parent == nil || *v.Parent != sender {
		t.Fatalf("turn 2 stop=%+v interruptions=%d parent=%+v",
			v.Stop, v.Interruptions, v.Parent)
	}
	if _, err := h.rec.Load(t.Context(), dashi.TurnID(childID(), 3)); err == nil {
		t.Fatal("Recover started a third turn")
	}

	usd := money.MustParseAmount
	v := h.tasks.List()[0]
	if v.Turns != 2 || v.TurnID != turn2 || v.Answer != "done" ||
		v.Spend.Cost["USD"] != usd("0.5") {
		t.Fatalf("view turns=%d turn=%q answer=%q spend=%v",
			v.Turns, v.TurnID, v.Answer, v.Spend.Cost)
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
	conv := &dashi.Session{New: true, Runner: h.runner, Agent: h.agent}
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
