package kernel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestContextInterruptLeavesToolBatchPending(t *testing.T) {
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
	def := definition()
	def.Limits.ToolConcurrency = 1
	finished := make(chan Outcome, 1)
	go func() {
		out, _ := (Engine{}).Apply(ctx, Turn{ID: "t1"}, def, Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Tools: fixedTools{first, second}, Recorder: rec, Policy: AllowLocalPolicy{}})
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
	if out.Stop.Kind != StopInterrupted || secondRuns != 0 {
		t.Fatalf("stop=%+v second runs=%d", out.Stop, secondRuns)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["a"].Status != ToolStarted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if status := view.Tools["b"].Status; status != ToolRequested &&
		status != ToolAllowed {
		t.Fatalf("unstarted call status=%s, want pending", status)
	}
	if len(out.Pending) != 2 || out.Pending[0] != "a" || out.Pending[1] != "b" {
		t.Fatalf("pending=%v", out.Pending)
	}
	late := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("late"))}
	settled, err := (Engine{}).Apply(context.Background(), out.Turn, def, Accept{ID: "late", CallID: "a", Outcome: &late}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if settled.Stop.Kind != StopInterrupted || len(settled.Pending) != 1 ||
		!settled.ReadyToContinue {
		t.Fatalf("late=%+v err=%v", settled, err)
	}
}

func TestInterruptedParallelBatchDoesNotStartLaterSibling(t *testing.T) {
	parts := []llm.Content{}
	for _, name := range []string{"first", "second", "third"} {
		call := tool.Call{ID: name, Name: name, Args: []byte(`{}`)}
		parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &call})
	}
	client := &scriptedClient{results: []llm.Result{{Message: llm.AssistantMessage(parts...), Stop: llm.Stop{Kind: llm.StopToolUse}}}}
	started := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	makeTool := func(name string) tool.Tool {
		return tool.Tool{Definition: tool.Definition{Name: name, Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			started <- name
			<-release
			return tool.Outcome{State: tool.StateSucceeded}, nil
		})}
	}
	var thirdRuns atomic.Int32
	third := tool.Tool{Definition: tool.Definition{Name: "third", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		thirdRuns.Add(1)
		return tool.Outcome{State: tool.StateSucceeded}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan Outcome, 1)
	rec := NewMemoryRecorder()
	go func() {
		out, _ := (Engine{}).Apply(ctx, Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Tools: fixedTools{makeTool("first"), makeTool("second"), third}, Recorder: rec, Policy: AllowLocalPolicy{}})
		finished <- out
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel tools did not both start")
		}
	}
	cancel()
	var out Outcome
	select {
	case out = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply waited for the parallel executors")
	}
	if out.Stop.Kind != StopInterrupted || thirdRuns.Load() != 0 ||
		len(out.Pending) != 3 {
		t.Fatalf("out=%+v third runs=%d", out, thirdRuns.Load())
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["first"].Status != ToolStarted ||
		view.Tools["second"].Status != ToolStarted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if status := view.Tools["third"].Status; status != ToolRequested &&
		status != ToolAllowed {
		t.Fatalf("unstarted call status=%s, want pending", status)
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

func TestInterruptedAfterModelIntentDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &scriptedClient{results: []llm.Result{final("must not send")}}
	rec := notifyingRecorder{Recorder: NewMemoryRecorder(), onCommit: func(step Step) {
		if step.Kind == StepModelIntent {
			cancel()
		}
	}}
	out, err := (Engine{}).Apply(ctx, Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil || out.Stop.Kind != StopInterrupted ||
		client.sends != 0 || len(out.Pending) != 0 {
		t.Fatalf("out=%+v err=%v sends=%d", out, err, client.sends)
	}
	settled := out.Turn.Steps[len(out.Turn.Steps)-2]
	if settled.Kind != StepModelSettled || !settled.Model.Interrupted {
		t.Fatalf("settlement=%+v", settled)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelAttempts != 0 {
		t.Fatalf("model attempts=%d, want 0", view.ModelAttempts)
	}
}

func TestInterruptedAfterModelResultDoesNotStartPendingTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "work")}}
	rec := notifyingRecorder{Recorder: NewMemoryRecorder(), onCommit: func(step Step) {
		if step.Kind == StepModelSettled {
			cancel()
		}
	}}
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "work", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		runs++
		return tool.Outcome{State: tool.StateSucceeded}, nil
	})}
	out, err := (Engine{}).Apply(ctx, Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil || out.Stop.Kind != StopInterrupted || runs != 0 ||
		client.sends != 1 || len(out.Pending) != 1 {
		t.Fatalf("out=%+v err=%v runs=%d sends=%d", out, err, runs, client.sends)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolRequested {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}
