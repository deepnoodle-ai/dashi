package kernel

import (
	"context"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestStartPinsScheduledOriginAndHistoryAcrossContinuation(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("first"), final("second")}}
	rec := NewMemoryRecorder()
	history := []llm.Message{llm.UserMessage(llm.Text("earlier request")), llm.AssistantMessage(llm.Text("earlier answer"))}
	start := Start{ID: "start", Origin: StartScheduled, History: history, Input: []llm.Message{llm.UserMessage(llm.Text("scheduled report"))}}
	first, err := (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(), start, Bindings{Client: client, Recorder: rec})
	if err != nil || first.Stop.Kind != StopCompleted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if first.Turn.Steps[0].Started.Origin != StartScheduled || len(first.Turn.Steps[0].Started.History) != 2 {
		t.Fatalf("start fact=%+v", first.Turn.Steps[0].Started)
	}
	history[0] = llm.UserMessage(llm.Text("changed outside turn"))
	projected, err := Project(first.Turn)
	if err != nil || len(projected) < 3 || projected[0].Content[0].Block.Text.Text != "earlier request" {
		t.Fatalf("projected=%+v err=%v", projected, err)
	}
	if _, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Start{ID: "start", Origin: StartScheduled, History: history, Input: start.Input}, Bindings{Client: client, Recorder: rec}); err != ErrCommandConflict {
		t.Fatalf("changed accepted history did not conflict: %v", err)
	}
}

func TestOperatorStartCanCarryTrustedRole(t *testing.T) {
	message := llm.UserMessage(llm.Text("run maintenance"))
	message.Role = llm.RoleDeveloper
	for _, tc := range []struct {
		origin StartOrigin
		valid  bool
	}{{StartOperator, true}, {StartScheduled, false}, {StartUser, false}} {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		out, err := (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(), Start{ID: "start", Origin: tc.origin, Input: []llm.Message{message}}, Bindings{Client: client, Recorder: NewMemoryRecorder()})
		if tc.valid && (err != nil || out.Stop.Kind != StopCompleted) {
			t.Fatalf("origin %q rejected: out=%+v err=%v", tc.origin, out, err)
		}
		if !tc.valid && (err == nil && out.Persistence == PersistenceAcknowledged) {
			t.Fatalf("origin %q accepted trusted role", tc.origin)
		}
	}
}
