package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type valueInput struct {
	Value int `json:"value"`
}

func TestToolFailureRecordShapes(t *testing.T) {
	oneCall := callResult("call-1", "remote")
	revoked := tool.Fatal(errors.New("credentials revoked"))
	invalid := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Outcome{State: tool.StateSucceeded}, nil
	})
	tests := []struct {
		name         string
		executor     tool.Executor
		results      []llm.Result
		stop         Stop
		status       ToolStatus
		state        tool.State
		reasonPrefix string
	}{
		{
			name:         "plain error",
			executor:     failingFunc(t, errors.New("quota exceeded")),
			results:      []llm.Result{oneCall, final("ok")},
			stop:         Stop{Kind: StopCompleted},
			status:       ToolSettled,
			state:        tool.StateFailed,
			reasonPrefix: "quota exceeded",
		},
		{
			name:         "fatal error",
			executor:     failingFunc(t, revoked),
			results:      []llm.Result{oneCall},
			stop:         failedStop(StopReasonToolFatal),
			status:       ToolSettled,
			state:        tool.StateFailed,
			reasonPrefix: "credentials revoked",
		},
		{
			name:         "invalid outcome from executor",
			executor:     invalid,
			results:      []llm.Result{oneCall},
			stop:         failedStop(StopReasonToolOutcomeInvalid),
			status:       ToolSettled,
			state:        tool.StateFailed,
			reasonPrefix: "tool: invalid outcome",
		},
		{
			name: "invalid outcome from func",
			executor: funcExecutor(t, func() (tool.Outcome, error) {
				return tool.Outcome{State: tool.StateSucceeded}, nil
			}),
			results:      []llm.Result{oneCall},
			stop:         failedStop(StopReasonToolOutcomeInvalid),
			status:       ToolSettled,
			state:        tool.StateFailed,
			reasonPrefix: "tool: invalid outcome",
		},
		{
			name: "panic",
			executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				panic("boom")
			}),
			results: []llm.Result{oneCall},
			stop: Stop{
				Kind:   StopUncertain,
				Reason: "tool outcome needs reconciliation",
			},
			status:       ToolUncertain,
			reasonPrefix: "panic: boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedClient{results: test.results}
			rec := NewMemoryRecorder()
			b := toolBindings(client, rec, test.executor)
			got, err := startTurn(definition(), b)
			if err != nil {
				t.Fatal(err)
			}
			if got.Stop != test.stop {
				t.Fatalf("stop=%+v, want %+v", got.Stop, test.stop)
			}
			view, err := Replay(got.Turn)
			if err != nil {
				t.Fatal(err)
			}
			if view.Tools["call-1"].Status != test.status {
				t.Fatalf("status=%s", view.Tools["call-1"].Status)
			}
			fact := settledFact(t, got.Turn, "call-1")
			if !strings.HasPrefix(fact.Reason, test.reasonPrefix) {
				t.Fatalf("recorded reason=%q", fact.Reason)
			}
			var state tool.State
			if fact.Outcome != nil {
				state = fact.Outcome.State
			}
			if state != test.state {
				t.Fatalf("recorded outcome state=%q, want %q", state, test.state)
			}
			assertRecordRoundTrips(t, rec, got.Turn)
		})
	}
}

func TestToolErrorReachesModelAsFailure(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("ok")},
	}
	executor := failingFunc(t, errors.New("quota exceeded"))
	b := toolBindings(client, NewMemoryRecorder(), executor)
	got, err := startTurn(definition(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted {
		t.Fatalf("turn did not continue: stop=%+v", got.Stop)
	}
	if client.sends != 2 {
		t.Fatalf("model calls=%d, want 2", client.sends)
	}
	result := toolResultFor(client.inputs[1], "call-1")
	if result == nil {
		t.Fatal("next model request lacks the tool result")
	}
	if !result.IsError {
		t.Fatal("tool error was not marked as an error")
	}
	if text := resultText(result); text != "quota exceeded" {
		t.Fatalf("model saw %q, want the error message", text)
	}
}

func TestFatalToolErrorStopsLaterCallsUntilContinue(t *testing.T) {
	def := definition()
	def.Limits.ToolConcurrency = 1
	client := &scriptedClient{
		results: []llm.Result{callsResult("a", "b"), final("ok")},
	}
	var runsA, runsB atomic.Int32
	work := func(_ context.Context, call tool.Call) (tool.Outcome, error) {
		if call.ID == "a" {
			runsA.Add(1)
			return tool.Outcome{}, tool.Fatal(errors.New("credentials revoked"))
		}
		runsB.Add(1)
		return succeeded("done"), nil
	}
	b := toolBindings(client, NewMemoryRecorder(), execFunc(work))
	first, err := startTurn(def, b)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stop != failedStop(StopReasonToolFatal) {
		t.Fatalf("stop=%+v", first.Stop)
	}
	if runsB.Load() != 0 {
		t.Fatal("a call started after a fatal tool error")
	}
	view, err := Replay(first.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if status := view.Tools["b"].Status; status != ToolAllowed {
		t.Fatalf("later call status=%s, want allowed", status)
	}
	resumed, err := apply(first.Turn, def, Continue{ID: "continue"}, b)
	if err != nil {
		t.Fatalf("continue after fatal tool error: %v", err)
	}
	if resumed.Stop.Kind != StopCompleted {
		t.Fatalf("resumed stop=%+v", resumed.Stop)
	}
	if runsA.Load() != 1 {
		t.Fatalf("fatal call ran %d times", runsA.Load())
	}
	if runsB.Load() != 1 {
		t.Fatalf("later call ran %d times", runsB.Load())
	}
	result := toolResultFor(client.inputs[1], "a")
	if result == nil || !result.IsError {
		t.Fatalf("model did not see the fatal failure: %+v", result)
	}
	if text := resultText(result); text != "credentials revoked" {
		t.Fatalf("model saw %q, want the error message", text)
	}
}

func TestToolPanicIsRecordedAsUncertain(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
	}{
		{name: "sequential", concurrency: 1},
		{name: "parallel", concurrency: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			def := definition()
			def.Limits.ToolConcurrency = test.concurrency
			client := &scriptedClient{
				results: []llm.Result{callsResult("a", "b"), final("ok")},
			}
			var runsA atomic.Int32
			work := func(_ context.Context, call tool.Call) (tool.Outcome, error) {
				if call.ID == "a" {
					runsA.Add(1)
					panic("boom")
				}
				return succeeded("done"), nil
			}
			b := toolBindings(client, NewMemoryRecorder(), execFunc(work))
			first, err := startTurn(def, b)
			if err != nil {
				t.Fatal(err)
			}
			if first.Stop.Kind != StopUncertain {
				t.Fatalf("stop=%+v", first.Stop)
			}
			view, err := Replay(first.Turn)
			if err != nil {
				t.Fatal(err)
			}
			if view.Tools["a"].Status != ToolUncertain {
				t.Fatalf("panicked call status=%s", view.Tools["a"].Status)
			}
			if view.Tools["b"].Status != ToolSettled {
				t.Fatalf("sibling call status=%s", view.Tools["b"].Status)
			}
			reason := settledFact(t, first.Turn, "a").Reason
			if !strings.HasPrefix(reason, "panic: boom\n") {
				t.Fatalf("panic reason=%q", reason)
			}
			continued := Continue{ID: "continue"}
			if _, err := apply(first.Turn, def, continued, b); err == nil {
				t.Fatal("continued past an unreconciled panic")
			}
			out := succeeded("verified")
			accept := Accept{ID: "reconcile", CallID: "a", Outcome: &out}
			accepted, err := apply(first.Turn, def, accept, b)
			if err != nil {
				t.Fatal(err)
			}
			if !accepted.ReadyToContinue || client.sends != 1 {
				t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
			}
			completed, err := apply(accepted.Turn, def, continued, b)
			if err != nil || completed.Stop.Kind != StopCompleted {
				t.Fatalf("continued=%+v err=%v", completed, err)
			}
			if runsA.Load() != 1 {
				t.Fatalf("panicked call ran %d times", runsA.Load())
			}
		})
	}
}

func TestReplayKeepsRecordedToolUncertainty(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote")},
	}
	executor := failingFunc(t, tool.Fatal(errors.New("connection lost")))
	b := toolBindings(client, NewMemoryRecorder(), executor)
	got, err := startTurn(definition(), b)
	if err != nil {
		t.Fatal(err)
	}
	// A record from an older engine, or a forged one, may claim uncertainty
	// for a returned error. Replay honors the record rather than guessing.
	forged := Turn{ID: got.Turn.ID}
	for _, step := range got.Turn.Steps {
		if step.Kind == StepToolSettled {
			break
		}
		forged.Steps = append(forged.Steps, step)
	}
	call := tool.Call{ID: "call-1", Name: "remote", Args: []byte(`{"value":1}`)}
	forged.Steps = append(forged.Steps, Step{
		Version: 1,
		TurnID:  forged.ID,
		Seq:     int64(len(forged.Steps) + 1),
		Kind:    StepToolSettled,
		Tool:    &ToolFact{Call: call, Reason: "connection lost"},
	})
	rec := NewMemoryRecorder()
	for _, step := range forged.Steps {
		req := CommitRequest{Step: step, ExpectedSeq: step.Seq - 1}
		if err := rec.Commit(context.Background(), req); err != nil {
			t.Fatalf("recorder rejected the uncertain record: %v", err)
		}
	}
	view, err := Replay(forged)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolUncertain {
		t.Fatalf("status=%s", view.Tools["call-1"].Status)
	}
	client = &scriptedClient{results: []llm.Result{final("ok")}}
	b = toolBindings(client, rec, executor)
	continued := Continue{ID: "continue"}
	if _, err := apply(forged, definition(), continued, b); err == nil {
		t.Fatal("continued past a recorded uncertain call")
	}
	out := tool.Outcome{
		State:  tool.StateFailed,
		Output: ptr(tool.TextOutput("connection lost")),
	}
	accept := Accept{ID: "reconcile", CallID: "call-1", Outcome: &out}
	accepted, err := apply(forged, definition(), accept, b)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.ReadyToContinue || client.sends != 0 {
		t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
	}
	completed, err := apply(accepted.Turn, definition(), continued, b)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("continued=%+v err=%v sends=%d", completed, err, client.sends)
	}
}

// startTurn starts turn "t1" with a fixed user input.
func startTurn(def Definition, b Bindings) (Outcome, error) {
	command := Start{
		ID:    "start",
		Input: []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}
	return apply(Turn{ID: "t1"}, def, command, b)
}

func apply(turn Turn, def Definition, command Command, b Bindings) (Outcome, error) {
	return (Engine{}).Apply(context.Background(), turn, def, command, b)
}

// toolBindings binds executor as the "remote" tool and allows local calls.
func toolBindings(client *scriptedClient, rec Recorder, executor tool.Executor) Bindings {
	return Bindings{
		Client:   client,
		Tools:    fixedTools{localTool("remote", executor)},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
}

func failedStop(reason string) Stop {
	return Stop{Kind: StopFailed, Reason: reason}
}

func succeeded(text string) tool.Outcome {
	output := tool.TextOutput(text)
	return tool.Outcome{State: tool.StateSucceeded, Output: &output}
}

// failingFunc builds a typed tool whose callback returns err.
func failingFunc(t *testing.T, err error) tool.Executor {
	t.Helper()
	return funcExecutor(t, func() (tool.Outcome, error) {
		return tool.Outcome{}, err
	})
}

// funcExecutor builds a typed tool whose callback returns fn's result, so
// tests exercise tool.Func's error pass-through.
func funcExecutor(t *testing.T, fn func() (tool.Outcome, error)) tool.Executor {
	t.Helper()
	callback := func(context.Context, tool.Call, valueInput) (tool.Outcome, error) {
		return fn()
	}
	built, err := tool.Func("remote", "", callback)
	if err != nil {
		t.Fatalf("construct typed tool: %v", err)
	}
	return built.Executor
}

// localTool binds executor to a definition whose schema accepts callResult's
// arguments.
func localTool(name string, executor tool.Executor) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   name,
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: executor,
	}
}

// callsResult is a model result requesting one "remote" call per ID.
func callsResult(ids ...string) llm.Result {
	parts := make([]llm.Content, 0, len(ids))
	for _, id := range ids {
		call := tool.Call{ID: id, Name: "remote", Args: []byte(`{"value":1}`)}
		parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &call})
	}
	return llm.Result{
		Message:   llm.AssistantMessage(parts...),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

// settledFact returns the recorded settlement for callID.
func settledFact(t *testing.T, turn Turn, callID string) *ToolFact {
	t.Helper()
	for _, step := range turn.Steps {
		if step.Kind == StepToolSettled && step.Tool.Call.ID == callID {
			return step.Tool
		}
	}
	t.Fatalf("no settled step for %s", callID)
	return nil
}

// toolResultFor returns the tool result for callID sent in a model request.
func toolResultFor(in llm.Input, callID string) *llm.ToolResult {
	for _, m := range in.Messages {
		for _, part := range m.Content {
			if part.ToolResult != nil && part.ToolResult.CallID == callID {
				return part.ToolResult
			}
		}
	}
	return nil
}

func resultText(result *llm.ToolResult) string {
	var out strings.Builder
	for _, block := range result.Output.Content {
		if block.Text != nil {
			out.WriteString(block.Text.Text)
		}
	}
	return out.String()
}

// assertRecordRoundTrips checks that the recorder holds the returned turn and
// that its JSON form replays to the same view.
func assertRecordRoundTrips(t *testing.T, rec *MemoryRecorder, turn Turn) {
	t.Helper()
	saved, err := rec.Load(context.Background(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatal("recorder holds a different turn than the outcome")
	}
	var decoded Turn
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := Replay(decoded)
	if err != nil {
		t.Fatalf("decoded record does not replay: %v", err)
	}
	expected, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if digest(got) != digest(expected) {
		t.Fatal("decoded record replays to a different view")
	}
}
