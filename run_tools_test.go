package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
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
			stop:         *testStop(StopCompleted, ""),
			status:       ToolFinished,
			state:        tool.StateFailed,
			reasonPrefix: "quota exceeded",
		},
		{
			name:         "fatal error",
			executor:     failingFunc(t, revoked),
			results:      []llm.Result{oneCall},
			stop:         failedStop(StopReasonToolFatal),
			status:       ToolFinished,
			state:        tool.StateFailed,
			reasonPrefix: "credentials revoked",
		},
		{
			name:         "invalid outcome from executor",
			executor:     invalid,
			results:      []llm.Result{oneCall},
			stop:         failedStop(StopReasonToolOutcomeInvalid),
			status:       ToolFinished,
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
			status:       ToolFinished,
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
				Kind:   StopResultUnknown,
				Reason: "tool outcome needs reconciliation",
			},
			status:       ToolResultUnknown,
			reasonPrefix: "panic: boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedClient{results: test.results}
			rec := NewMemoryRecorder()
			runner := toolRunner(client, rec, test.executor)
			got, err := startTurn(testAgent(), runner)
			if err != nil {
				t.Fatal(err)
			}
			if got.Stop != test.stop {
				t.Fatalf("stop=%+v, want %+v", got.Stop, test.stop)
			}
			view, err := got.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			if view.Tools["call-1"].Status != test.status {
				t.Fatalf("status=%s", view.Tools["call-1"].Status)
			}
			fact := finishedFact(t, got.Turn, "call-1")
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
	runner := toolRunner(client, NewMemoryRecorder(), executor)
	got, err := startTurn(testAgent(), runner)
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
	def := testAgent()
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
	runner := toolRunner(client, NewMemoryRecorder(), execFunc(work))
	first, err := startTurn(def, runner)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stop != failedStop(StopReasonToolFatal) {
		t.Fatalf("stop=%+v", first.Stop)
	}
	if runsB.Load() != 0 {
		t.Fatal("a call started after a fatal tool error")
	}
	view, err := first.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if status := view.Tools["b"].Status; status != ToolAllowed {
		t.Fatalf("later call status=%s, want allowed", status)
	}
	resumed, err := apply(first.Turn, Continue{CommandID: "continue"}, runner)
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

func TestToolPanicIsRecordedAsResultUnknown(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
	}{
		{name: "sequential", concurrency: 1},
		{name: "parallel", concurrency: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			def := testAgent()
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
			runner := toolRunner(client, NewMemoryRecorder(), execFunc(work))
			first, err := startTurn(def, runner)
			if err != nil {
				t.Fatal(err)
			}
			if first.Stop.Kind != StopResultUnknown {
				t.Fatalf("stop=%+v", first.Stop)
			}
			view, err := first.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			if view.Tools["a"].Status != ToolResultUnknown {
				t.Fatalf("panicked call status=%s", view.Tools["a"].Status)
			}
			if view.Tools["b"].Status != ToolFinished {
				t.Fatalf("sibling call status=%s", view.Tools["b"].Status)
			}
			reason := finishedFact(t, first.Turn, "a").Reason
			if !strings.HasPrefix(reason, "panic: boom\n") {
				t.Fatalf("panic reason=%q", reason)
			}
			continued := Continue{CommandID: "continue"}
			if _, err := apply(first.Turn, continued, runner); err == nil {
				t.Fatal("continued past an unreconciled panic")
			}
			out := succeeded("verified")
			finish := Resolve{CommandID: "reconcile", CallID: "a", Outcome: &out}
			accepted, err := apply(first.Turn, finish, runner)
			if err != nil {
				t.Fatal(err)
			}
			if !accepted.ReadyToContinue || client.sends != 1 {
				t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
			}
			completed, err := apply(accepted.Turn, continued, runner)
			if err != nil || completed.Stop.Kind != StopCompleted {
				t.Fatalf("continued=%+v err=%v", completed, err)
			}
			if runsA.Load() != 1 {
				t.Fatalf("panicked call ran %d times", runsA.Load())
			}
		})
	}
}

func TestReplayKeepsRecordedToolUnknownResult(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote")},
	}
	executor := failingFunc(t, tool.Fatal(errors.New("connection lost")))
	runner := toolRunner(client, NewMemoryRecorder(), executor)
	got, err := startTurn(testAgent(), runner)
	if err != nil {
		t.Fatal(err)
	}
	// A record from an older runner, or a forged one, may claim an unknown
	// result for a returned error. View honors the record rather than
	// guessing.
	forged := Turn{ID: got.Turn.ID}
	for _, step := range got.Turn.Steps {
		if step.Kind == StepToolFinished {
			break
		}
		forged.Steps = append(forged.Steps, step)
	}
	call := tool.Call{ID: "call-1", Name: "remote", Args: []byte(`{"value":1}`)}
	forged.Steps = append(
		forged.Steps,
		Step{
			Version: 1,
			TurnID:  forged.ID,
			Seq:     int64(len(forged.Steps) + 1),
			Kind:    StepToolFinished,
			Tool:    &ToolFact{Call: call, Reason: "connection lost"},
		},
	)
	rec := NewMemoryRecorder()
	for _, step := range forged.Steps {
		req := CommitRequest{Step: step, ExpectedSeq: step.Seq - 1}
		if err := rec.Commit(t.Context(), req); err != nil {
			t.Fatalf("recorder rejected the uncertain record: %v", err)
		}
	}
	view, err := forged.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolResultUnknown {
		t.Fatalf("status=%s", view.Tools["call-1"].Status)
	}
	client = &scriptedClient{results: []llm.Result{final("ok")}}
	runner = toolRunner(client, rec, executor)
	continued := Continue{CommandID: "continue"}
	if _, err := apply(forged, continued, runner); err == nil {
		t.Fatal("continued past a recorded uncertain call")
	}
	out := tool.Outcome{
		State:  tool.StateFailed,
		Output: new(tool.TextOutput("connection lost")),
	}
	finish := Resolve{CommandID: "reconcile", CallID: "call-1", Outcome: &out}
	accepted, err := apply(forged, finish, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.ReadyToContinue || client.sends != 0 {
		t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
	}
	completed, err := apply(accepted.Turn, continued, runner)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("continued=%+v err=%v sends=%d", completed, err, client.sends)
	}
}

// startTurn starts turn "t1" with a fixed user input.
func startTurn(def Agent, runner Runner) (RunResult, error) {
	command := Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}
	return apply(Turn{ID: "t1"}, command, runner)
}

func apply(turn Turn, command Command, runner Runner) (RunResult, error) {
	return runner.Run(context.Background(), turn, command)
}

// toolRunner binds executor as the "remote" tool and allows local calls.
func toolRunner(
	client *scriptedClient,
	rec Recorder,
	executor tool.Executor,
) Runner {
	return Runner{
		Client:   client,
		Tools:    tool.List{localTool("remote", executor)},
		Recorder: rec,
	}
}

func failedStop(reason string) Stop {
	return Stop{Kind: StopFailed, Reason: reason}
}

func succeeded(text string) tool.Outcome {
	return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput(text))}
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
func funcExecutor(
	t *testing.T,
	fn func() (tool.Outcome, error),
) tool.Executor {
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
		parts = append(
			parts,
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
		)
	}
	return llm.Result{
		Message:   llm.AssistantMessage(parts...),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

// finishedFact returns the recorded finish fact for callID.
func finishedFact(t *testing.T, turn Turn, callID string) *ToolFact {
	t.Helper()
	for _, step := range turn.Steps {
		if step.Kind == StepToolFinished && step.Tool.Call.ID == callID {
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
// that its JSON form builds the same view.
func assertRecordRoundTrips(t *testing.T, rec *MemoryRecorder, turn Turn) {
	t.Helper()
	saved, err := rec.Load(t.Context(), turn.ID)
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
	got, err := decoded.View()
	if err != nil {
		t.Fatalf("decoded record does not replay: %v", err)
	}
	expected, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if canonjson.Hash(got) != canonjson.Hash(expected) {
		t.Fatal("decoded record replays to a different view")
	}
}

func TestContinueRepeatsOnlySafeStartedLocalTools(t *testing.T) {
	tests := []struct {
		name        string
		annotations tool.Annotations
		wantRuns    int
		wantStop    StopKind
	}{
		{
			name:        "read only",
			annotations: tool.Annotations{ReadOnly: new(true)},
			wantRuns:    1,
			wantStop:    StopCompleted,
		},
		{
			name:        "idempotent",
			annotations: tool.Annotations{Idempotent: new(true)},
			wantRuns:    1,
			wantStop:    StopCompleted,
		},
		{name: "unannotated", wantStop: StopResultUnknown},
		{
			name: "explicitly not safe",
			annotations: tool.Annotations{
				ReadOnly:   new(false),
				Idempotent: new(false),
			},
			wantStop: StopResultUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toolDef := tool.Definition{
				Name:        "work",
				Schema:      []byte(`{"type":"object"}`),
				Kind:        tool.KindLocal,
				Annotations: tc.annotations,
			}
			rec := NewMemoryRecorder()
			turn := startedLocalToolTurn(t, rec, toolDef)
			client := &scriptedClient{results: []llm.Result{final("done")}}
			runs := 0
			bound := tool.Tool{
				Definition: toolDef,
				Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
					runs++
					return tool.Outcome{
						State:  tool.StateSucceeded,
						Output: new(tool.TextOutput("done")),
					}, nil
				}),
			}
			out, err := (Runner{
				Client:   client,
				Tools:    tool.List{bound},
				Recorder: rec,
			}).Run(t.Context(), turn, Continue{CommandID: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != tc.wantStop {
				t.Fatalf("stop=%+v; want %s", out.Stop, tc.wantStop)
			}
			if runs != tc.wantRuns {
				t.Fatalf("tool ran %d times; want %d", runs, tc.wantRuns)
			}
			starts := 0
			for _, step := range out.Turn.Steps {
				if step.Kind == StepToolStarted {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("record contains %d tool starts; want one", starts)
			}
		})
	}
}

func TestPauseDuringToolLoopFinishesRunningCallBeforeStopping(t *testing.T) {
	firstCall := tool.Call{ID: "first", Name: "first", Args: []byte(`{}`)}
	secondCall := tool.Call{ID: "second", Name: "second", Args: []byte(`{}`)}
	client := &scriptedClient{
		results: []llm.Result{
			{
				Message: llm.AssistantMessage(
					llm.Content{Kind: llm.ContentToolCall, ToolCall: &firstCall},
					llm.Content{Kind: llm.ContentToolCall, ToolCall: &secondCall},
				),
				Stop: llm.Stop{Kind: llm.StopToolUse},
			},
			final("done"),
		},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	pause := make(chan struct{})
	first := tool.Tool{
		Definition: tool.Definition{
			Name:   "first",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			close(started)
			<-release
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("first result")),
			}, nil
		}),
	}
	secondRuns := 0
	second := tool.Tool{
		Definition: tool.Definition{
			Name:   "second",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			secondRuns++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("second result")),
			}, nil
		}),
	}
	def := testAgent()
	def.Limits.ToolConcurrency = 1
	rec := NewMemoryRecorder()
	type result struct {
		out RunResult
		err error
	}
	finished := make(chan result, 1)
	go func() {
		out, err := (Runner{
			Client:   client,
			Tools:    tool.List{first, second},
			Recorder: rec,
		}).Run(
			t.Context(),
			Turn{ID: "t1"},
			Start{
				CommandID: "start",
				Agent:     def,
				Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
			},
			WithPause(pause),
		)
		finished <- result{out: out, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first tool did not start")
	}
	close(pause)
	close(release)
	var got result
	select {
	case got = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not pause")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.out.Stop.Kind != StopInterrupted ||
		got.out.Stop.Reason != "pause requested" {
		t.Fatalf("stop=%+v", got.out.Stop)
	}
	if secondRuns != 0 {
		t.Fatalf("second tool ran %d times before resume", secondRuns)
	}
	view, err := got.out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["first"].Status != ToolFinished {
		t.Fatalf("first tool status=%s; want settled", view.Tools["first"].Status)
	}
	secondStatus := view.Tools["second"].Status
	if secondStatus != ToolRequested && secondStatus != ToolAllowed {
		t.Fatalf("second tool status=%s; want pending", secondStatus)
	}
	resumed, err := (Runner{
		Client:   client,
		Tools:    tool.List{first, second},
		Recorder: rec,
	}).Run(t.Context(), got.out.Turn, Continue{CommandID: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind != StopCompleted || secondRuns != 1 {
		t.Fatalf("resume stop=%+v; second runs=%d", resumed.Stop, secondRuns)
	}
}

func TestToolResultAfterContextEndIsDropped(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "work")}}
	rec := NewMemoryRecorder()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	returned := make(chan struct{})
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "work",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			cancel(errors.New("lease lost"))
			defer close(returned)
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("late result")),
			}, nil
		}),
	}
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-returned
	if out.Stop.Kind != StopInterrupted || out.Stop.Reason != "lease lost" {
		t.Fatalf("stop=%+v", out.Stop)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := saved.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolStarted {
		t.Fatalf("tool status=%s; want started", view.Tools["call-1"].Status)
	}
	for _, step := range saved.Steps {
		if step.Kind == StepToolFinished && step.Tool.Call.ID == "call-1" {
			t.Fatal("late tool result was committed")
		}
	}
}

func startedLocalToolTurn(
	t *testing.T,
	rec *MemoryRecorder,
	toolDef tool.Definition,
) Turn {
	t.Helper()
	def := testAgent()
	turn := Turn{ID: "t1"}
	commit := func(step Step) {
		t.Helper()
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(
			t.Context(),
			CommitRequest{Step: step, ExpectedSeq: step.Seq - 1},
		); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	call := tool.Call{ID: "call-1", Name: "work", Args: []byte(`{}`)}
	commit(Step{
		Kind: StepTurnStarted,
		Started: &StartFact{
			Agent: def,
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	})
	commit(Step{
		Kind: StepModelCallStarted,
		Model: &ModelFact{
			ModelCallID: "t1:model:1",
			MessageID:   "t1:model:1:message",
			RequestHash: "digest",
			Provider:    def.Provider,
			API:         def.API,
			Model:       def.Model,
			Tools:       []tool.Definition{toolDef},
		},
	})
	requested := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: def.Provider, Model: def.Model},
	}
	commit(Step{
		Kind: StepModelCallFinished,
		Model: &ModelFact{
			ModelCallID: "t1:model:1",
			MessageID:   "t1:model:1:message",
			Result:      &requested,
		},
	})
	commit(Step{
		Kind: StepToolDecision,
		Tool: &ToolFact{
			Call:       call,
			Definition: toolDef,
			Verdict:    ToolVerdict{Kind: DecisionAllow, RulesVersion: "r1"},
		},
	})
	commit(Step{
		Kind: StepToolStarted,
		Tool: &ToolFact{Call: call, Definition: toolDef},
	})
	return turn
}
