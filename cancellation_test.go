package kernel

import (
	"context"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestCanceledContextStopsToolBatchWithoutWaitingForExecutor(t *testing.T) {
	a := tool.Call{ID: "a", Name: "first", Args: []byte(`{}`)}
	b := tool.Call{ID: "b", Name: "second", Args: []byte(`{}`)}
	client := &scriptedClient{results: []llm.Result{{Message: llm.AssistantMessage(
		llm.Content{Kind: llm.ContentToolCall, ToolCall: &a},
		llm.Content{Kind: llm.ContentToolCall, ToolCall: &b}), Stop: llm.Stop{Kind: llm.StopToolUse}}}}
	rec := NewMemoryRecorder()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	first := tool.Tool{Definition: tool.Definition{Name: "first", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		close(started)
		<-release // Deliberately ignores cancellation.
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("late"))}, nil
	})}
	secondRuns := 0
	second := tool.Tool{Definition: tool.Definition{Name: "second", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		secondRuns++
		return tool.Outcome{State: tool.StateSucceeded}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan Outcome, 1)
	go func() {
		out, _ := (Engine{}).Apply(ctx, Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Tools: fixedTools{first, second}, Recorder: rec, Policy: AllowLocalPolicy{}})
		finished <- out
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first executor did not start")
	}
	cancel()
	var out Outcome
	select {
	case out = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply waited for an executor that ignored cancellation")
	}
	if out.Stop.Kind != StopCanceled || secondRuns != 0 {
		t.Fatalf("stop=%+v second runs=%d", out.Stop, secondRuns)
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Tools["a"].Status != ToolStarted || view.Tools["b"].Status != ToolSettled || view.Tools["b"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if len(out.Pending) != 1 || out.Pending[0] != "a" {
		t.Fatalf("pending=%v", out.Pending)
	}
	late := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("late"))}
	settled, err := (Engine{}).Apply(context.Background(), out.Turn, definition(), Accept{ID: "late", CallID: "a", Outcome: &late}, Bindings{Client: client, Recorder: rec})
	if err != nil || settled.Stop.Kind != StopCanceled || len(settled.Pending) != 0 {
		t.Fatalf("late=%+v err=%v", settled, err)
	}
}

func TestExplicitCancelSettlesUnstartedApproval(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, fixedTools{tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}}}, rec, deferredPolicy{})
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", first, err)
	}
	canceled, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Cancel{ID: "cancel"}, Bindings{Client: client, Recorder: rec})
	if err != nil || canceled.Stop.Kind != StopCanceled || len(canceled.Pending) != 0 {
		t.Fatalf("cancel=%+v err=%v", canceled, err)
	}
	view, err := Replay(canceled.Turn)
	if err != nil || view.Tools["call-1"].Status != ToolSettled || view.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestCanceledAfterModelIntentDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &scriptedClient{results: []llm.Result{final("must not send")}}
	rec := notifyingRecorder{Recorder: NewMemoryRecorder(), onCommit: func(step Step) {
		if step.Kind == StepModelIntent {
			cancel()
		}
	}}
	out, err := (Engine{}).Apply(ctx, Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil || out.Stop.Kind != StopCanceled || client.sends != 0 || len(out.Pending) != 0 {
		t.Fatalf("out=%+v err=%v sends=%d", out, err, client.sends)
	}
	settled := out.Turn.Steps[len(out.Turn.Steps)-2]
	if settled.Kind != StepModelSettled || settled.Model.Failure != llm.FailureNotDispatched {
		t.Fatalf("settlement=%+v", settled)
	}
}
