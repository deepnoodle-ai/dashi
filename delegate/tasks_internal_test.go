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
// policy still reports unresolved attention, then resolves the child without
// announcing a second result under the same NoticeID.
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
	task, _, err := ts.register(ctx, Subagent{Name: "worker"}, ref, "change", runner, rec, map[string]bool{child: true})
	if err != nil {
		t.Fatal(err)
	}
	idle, err := ts.Wait(ctx)
	var attention *AttentionError
	if idle || !errors.As(err, &attention) {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
	if _, err = ts.Run(ctx, TaskTarget{ID: task.id, TurnID: child, Revision: out.Revision}, dashi.Cancel{CommandID: "discard", Reason: "operator discarded"}); err != nil {
		t.Fatal(err)
	}
	if idle, err = ts.Wait(ctx); !idle || err != nil {
		t.Fatalf("resolved idle=%v err=%v", idle, err)
	}
	if notices.Load() != 0 {
		t.Fatal("rewrote an acknowledged historical notice")
	}
}

type internalWaiting struct{}

func (internalWaiting) Execute(context.Context, tool.Call) (tool.Outcome, error) {
	return tool.Waiting("external worker"), nil
}
