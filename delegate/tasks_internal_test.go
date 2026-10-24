package delegate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestMaxTaskTurnsBoundsTheTurnsOfATask(t *testing.T) {
	for _, tc := range []struct {
		max, turns int
		refused    bool
	}{
		{max: 0, turns: 9},
		{max: 0, turns: 10, refused: true},
		{max: 2, turns: 1},
		{max: 2, turns: 2, refused: true},
		{max: -1, turns: 100},
	} {
		ts := NewTasks(Config{MaxTaskTurns: tc.max})
		task := newTask(Subagent{Name: "scout"}, dashi.CallRef{TurnID: "p", CallID: "c"}, "")
		for range tc.turns {
			task.turns = append(task.turns, &childTurn{state: "completed"})
		}
		err := ts.checkTurnsLocked(task)
		if tc.refused != (err != nil) {
			t.Fatalf("max=%d turns=%d: err=%v", tc.max, tc.turns, err)
		}
		if err != nil && !strings.Contains(err.Error(), "start a new task") {
			t.Fatalf("error does not suggest a new task: %v", err)
		}
	}
}

// A notice acknowledged by an older host is immutable. The new readiness
// policy still reports unresolved attention. Resolving the child is a new
// stop, so it is announced once, under its own notice ID, never under the
// acknowledged NoticeID.
func TestAcknowledgedHistoricalNoticeStillRequiresAttention(t *testing.T) {
	ctx := t.Context()
	recorder := dashi.NewMemoryRecorder()
	ref := dashi.CallRef{TurnID: "parent", CallID: "task-call"}
	child := session.TurnID(ChildID(ref.TurnID, ref.CallID), 1)
	runner := dashi.Runner{Recorder: recorder, Client: dashitest.NewClient(dashitest.CallTool("write", "external", `{}`)), Tools: tool.List{{
		Definition: tool.Definition{Name: "external", Description: "External work", Schema: []byte(`{"type":"object"}`)},
		Executor:   internalWaiting{},
	}}}
	out, err := runner.Run(ctx, dashi.Turn{ID: child}, dashi.Start{Parent: &ref, Agent: dashi.Agent{Model: dashitest.Model}, Input: []llm.Message{llm.UserMessage(llm.Text("apply change"))}})
	if err != nil || out.Stop.Kind != dashi.StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	var notices atomic.Int32
	ts := NewTasks(Config{NoticeReady: func(v dashi.TurnView) bool {
		return v.Stop.Ended || v.Stop.Kind == dashi.StopCompleted || v.Stop.Kind == dashi.StopCanceled
	}, OnTask: func(e TaskEvent) {
		if e.Kind == TaskEnded {
			notices.Add(1)
		}
	}})
	defer ts.Close(ctx)
	rec, err := loadRecord(ctx, recorder, ChildID(ref.TurnID, ref.CallID))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := ts.register(ctx, Subagent{Name: "worker"}, ref, "change", runner, rec, map[string]bool{NoticeID(child): true})
	if err != nil {
		t.Fatal(err)
	}
	idle, err := ts.Wait(ctx)
	var attention *AttentionError
	if idle || !errors.As(err, &attention) {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
	if _, err = ts.Run(ctx, TaskTarget{ID: task.id, TurnID: child, LastSeq: out.LastSeq}, dashi.Cancel{CommandID: "discard", Reason: "operator discarded"}); err != nil {
		t.Fatal(err)
	}
	// The cancel is a new stop: Wait reports it once, then is idle.
	if idle, err = ts.Wait(ctx); idle || err != nil {
		t.Fatalf("resolved idle=%v err=%v; want one report", idle, err)
	}
	if idle, err = ts.Wait(ctx); !idle || err != nil {
		t.Fatalf("reported idle=%v err=%v", idle, err)
	}
	if n := notices.Load(); n != 1 {
		t.Fatalf("TaskEnded events = %d; want 1 for the cancel", n)
	}
	if id := ts.List()[0].NoticeID; id == NoticeID(child) || !strings.HasPrefix(id, NoticeID(child)+"@") {
		t.Fatalf("notice ID %q reuses the acknowledged notice's ID", id)
	}
}

type internalWaiting struct{}

func (internalWaiting) Execute(context.Context, tool.Call) (tool.Outcome, error) {
	return tool.Waiting("external worker"), nil
}

// A child turn interrupted while Tasks is open, such as by a pause, is an
// end like any other: its notice is reported, and mail that missed it
// starts the next turn, so Wait does not count the mail forever. Close
// interrupts silently and leaves the mail for Recover.
func TestInterruptionOutsideCloseReportsAndPassesMailOn(t *testing.T) {
	for _, closed := range []bool{false, true} {
		ts := NewTasks(Config{})
		ref := dashi.CallRef{TurnID: "p", CallID: "c"}
		task := newTask(Subagent{Name: "scout"}, ref, "")
		id := session.TurnID(task.sessionID, 1)
		task.turns = []*childTurn{{id: id, state: TaskRunning}}
		task.mail = []*mail{{msg: Message{ID: MessageID("p", "m"), Text: "more"}, call: ref}}
		ts.tasks[task.id] = task
		ts.closed = closed
		ts.endTurn(task, id, dashi.RunResult{Stop: dashi.Stop{Kind: dashi.StopInterrupted}}, nil)
		ts.mu.Lock()
		c := task.turns[0]
		next := ts.passOnLocked(task)
		ts.mu.Unlock()
		if c.suppressed != closed || c.reported != closed {
			t.Fatalf("closed=%v: suppressed=%v reported=%v", closed, c.suppressed, c.reported)
		}
		if closed != (next == nil) {
			t.Fatalf("closed=%v: next turn %v", closed, next)
		}
		if start, ok := next.(dashi.Start); !closed && (!ok || start.CommandID != MessageID("p", "m")) {
			t.Fatalf("next turn %#v", next)
		}
	}
}

func TestIsTurnOfMatchesWholeTurnNumbers(t *testing.T) {
	call := ChildID("p", "call")
	sibling := ChildID("p", "call-1")
	for _, tc := range []struct {
		turn, session string
		want          bool
	}{
		{session.TurnID(call, 1), call, true},
		{session.TurnID(call, 12), call, true},
		{session.TurnID(sibling, 1), call, false},
		{session.TurnID(sibling, 1), sibling, true},
		{call + "-0", call, false},
		{call + "-01", call, false},
		{call + "-", call, false},
		{call, call, false},
	} {
		if got := isTurnOf(tc.turn, tc.session); got != tc.want {
			t.Errorf("isTurnOf(%q, %q) = %v", tc.turn, tc.session, got)
		}
	}
}

// NewTasks keeps a Delivered function the host set and adds the notices
// and messages Tasks needs.
func TestNewTasksExtendsTheSessionsDelivered(t *testing.T) {
	msg := MessageID("child-1:p:c-1", "call")
	s := &session.Session{Delivered: func([]dashi.Turn) map[string]bool {
		return map[string]bool{"host-item": true}
	}}
	NewTasks(Config{Session: s})
	got := s.Delivered([]dashi.Turn{{Steps: []dashi.Step{{
		Kind: dashi.StepTurnStarted, CommandID: msg,
	}}}})
	if !got["host-item"] || !got[msg] {
		t.Fatalf("delivered = %v", got)
	}
}
