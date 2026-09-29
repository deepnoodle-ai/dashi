package dashi

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func limitedForAttempts(stop Stop) bool {
	return stop.Kind == StopLimited && stop.Reason == "model_attempts" &&
		stop.Limit != nil && stop.Limit.Kind == LimitModelAttempts
}

func TestReservedFinalAnswerUsesLastAttemptWithoutExtraCall(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelAttempts: 2}
	def.Settings.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAny}
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			final("Found 42; further work is unfinished."),
		},
	}
	runs := 0
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("42")),
			}, nil
		}),
	}
	rec := NewMemoryRecorder()
	out, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || !limitedForAttempts(out.Stop) ||
		out.Answer != "Found 42; further work is unfinished." || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v err=%v sends=%d runs=%d", out, err, client.sends, runs)
	}
	if len(client.inputs) != 2 || len(client.inputs[0].Tools) != 1 || len(client.inputs[1].Tools) != 0 ||
		client.inputs[1].Settings.ToolChoice != nil {
		t.Fatalf("model inputs=%+v", client.inputs)
	}
	if client.inputs[1].System == "" || client.inputs[1].System == client.inputs[0].System {
		t.Fatal("final-answer instruction was not sent")
	}
	intents := 0
	for _, step := range out.Turn.Steps {
		if step.Kind != StepModelIntent {
			continue
		}
		intents++
		if step.Model.FinalAnswerOnly != (intents == 2) || (intents == 2 && len(step.Model.Tools) != 0) {
			t.Fatalf("intent %d=%+v", intents, step.Model)
		}
	}
	view, err := Replay(out.Turn)
	if err != nil || view.ModelAttempts != 2 || view.Stop.Kind != StopLimited {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	again, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		out.Turn,
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || again.Stop.Kind != StopLimited || client.sends != 2 || runs != 1 {
		t.Fatalf(
			"duplicate start=%+v err=%v sends=%d runs=%d",
			again,
			err,
			client.sends,
			runs,
		)
	}
	if _, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(t.Context(), out.Turn, Continue{CommandID: "too-late"}); err == nil {
		t.Fatal("exhausted turn accepted a continuation")
	}
}

func TestReservedFinalAnswerKeepsStructuredOutputSubmission(t *testing.T) {
	def := structuredAgent()
	def.Limits.ModelAttempts = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	binding := answerLookup()
	out, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || !limitedForAttempts(out.Stop) ||
		out.Output == nil || out.Output.Source != OutputFromTool || string(out.Output.Value) != `{"answer":42}` || client.sends != 2 {
		t.Fatalf("out=%+v err=%v sends=%d", out, err, client.sends)
	}
	if len(client.inputs[1].Tools) != 1 || client.inputs[1].Tools[0].Name != SubmitOutputTool {
		t.Fatalf("final tools=%+v", client.inputs[1].Tools)
	}
	if !json.Valid(out.Output.Value) {
		t.Fatalf("structured output=%+v", out.Output)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent && step.Model.FinalAnswerOnly {
			step.Model.Tools = append(step.Model.Tools, binding.Definition)
			if _, err := Replay(out.Turn); err == nil {
				t.Fatal("replay accepted an application tool on the final-answer request")
			}
			return
		}
	}
	t.Fatal("missing final-answer intent")
}

func TestReplayAllowsDeadlineAfterRecordedOutput(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	i := slices.IndexFunc(out.Turn.Steps, func(step Step) bool {
		return step.Kind == StepOutputRecorded
	})
	if i < 0 {
		t.Fatal("no recorded output")
	}
	recorded := out.Turn.Steps[i]

	tests := []struct {
		name    string
		stop    Stop
		allowed bool
	}{
		{
			name: "model attempts without final attempt",
			stop: *testStop(StopLimited, LimitModelAttempts),
		},
		{
			name:    "deadline",
			stop:    *testStop(StopLimited, LimitDeadline),
			allowed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := tt.stop
			turn := out.Turn
			turn.Steps = append(
				slices.Clone(out.Turn.Steps[:i+1]),
				Step{
					Version: recorded.Version,
					TurnID:  recorded.TurnID,
					Seq:     int64(i + 2),
					Kind:    StepStopped,
					Stop:    &stop,
				},
			)
			_, err := Replay(turn)
			if (err == nil) != tt.allowed {
				t.Fatalf("replay of %+v: allowed=%t err=%v", stop, tt.allowed, err)
			}
		})
	}

	def := structuredAgent()
	def.Limits.ModelAttempts = 2
	final := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	start := Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
	}
	runner := Runner{
		Agent:    def,
		Client:   final,
		Tools:    tool.List{answerLookup()},
		Recorder: NewMemoryRecorder(),
	}
	limited, err := runner.Run(
		t.Context(),
		Turn{ID: "limit"},
		start,
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(limited.Turn)
	if err != nil {
		t.Fatalf("replay rejected limited stop after final attempt: %v", err)
	}
	if !limitedForAttempts(view.Stop) {
		t.Fatalf("stop=%+v", view.Stop)
	}
}

func TestReservedFinalAnswerCannotExecuteUnadvertisedTool(t *testing.T) {
	def := testAgent()
	def.Limits.ModelAttempts = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("first", "lookup"),
			callResult("second", "lookup"),
		},
	}
	runs := 0
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
	out, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up"))},
		},
	)
	if err != nil || out.Stop.Kind != StopLimited || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v err=%v sends=%d runs=%d", out, err, client.sends, runs)
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Tools["second"].Status != ToolSettled ||
		view.Tools["second"].Outcome == nil || view.Tools["second"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
}

func TestReservedFinalAnswerClassifiesAfterStopWriteFailure(t *testing.T) {
	def := testAgent()
	def.Limits.ModelAttempts = 2
	client := &scriptedClient{
		results: []llm.Result{callResult("first", "lookup"), final("42")},
	}
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("42")),
			}, nil
		}),
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepStopped}
	first, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up"))},
		},
	)
	if persistence(err) != PersistenceRejected || client.sends != 2 {
		t.Fatalf("first=%+v err=%v sends=%d", first, err, client.sends)
	}
	saved, err := base.Load(t.Context(), "limit")
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(t.Context(), saved, Continue{CommandID: "recover"})
	if err != nil || !limitedForAttempts(second.Stop) ||
		second.Answer != "42" || client.sends != 2 {
		t.Fatalf("recovered=%+v err=%v sends=%d", second, err, client.sends)
	}
}

// answerLookup is a local tool that always answers 42.
func answerLookup() tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("42"))}, nil
		}),
	}
}

func TestContinueRaisesModelAttemptLimit(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelAttempts: 2, NoReservedFinalAnswer: true}
	client := &scriptedClient{
		results: []llm.Result{
			callResult("c1", "lookup"),
			callResult("c2", "lookup"),
			callResult("c3", "lookup"),
			final("done"),
		},
	}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		}),
	}
	runner := Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{lookup},
		Recorder: NewMemoryRecorder(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "limit"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil || !limitedForAttempts(out.Stop) || out.Stop.Terminal {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}

	// Without a raise, or with a raise that does not exceed the limit, the
	// turn does not continue.
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID: "plain",
	}); err == nil {
		t.Fatal("continue without a raise ran a spent turn")
	}
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID:     "same",
		ModelAttempts: 2,
	}); err == nil {
		t.Fatal("a raise equal to the limit was accepted")
	}

	// The raise keeps the same turn and its record.
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:     "raise-1",
		ModelAttempts: 3,
	})
	if err != nil || !limitedForAttempts(out.Stop) || out.Turn.ID != "limit" {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if out.Stop.Limit.Limit != 3 || out.Stop.Limit.Used != 3 {
		t.Fatalf("limit detail=%+v", out.Stop.Limit)
	}

	// A second raise builds on the first, and the record replays.
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID:     "lower",
		ModelAttempts: 3,
	}); err == nil {
		t.Fatal("a raise equal to the raised limit was accepted")
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:     "raise-2",
		ModelAttempts: 10,
	})
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "done" {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
	}
	if _, err := Replay(out.Turn); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestBudgetRaiseAloneCannotRunToolsWithoutModelCalls(t *testing.T) {
	s := &replayState{}
	s.Revision, s.ModelAttempts, s.attemptCap = 2, 3, 3
	s.Stop = Stop{
		Kind:  StopLimited,
		Limit: &LimitDetail{Kind: LimitTotalTokens},
	}
	s.Tools = map[string]ToolView{"c1": {Status: ToolRequested}}
	s.callOrder = []string{"c1"}
	raise := &Budget{MaxTotalTokens: 1 << 40}
	if s.canContinueWith(raise, 0) || s.canContinueWith(raise, 3) {
		t.Fatal("a budget raise without a model-call raise was accepted")
	}
	if !s.canContinueWith(raise, 4) {
		t.Fatal("a budget raise with room for a model call was rejected")
	}
}

func TestStructuredAnswerOnFinalCallCompletesAfterContinue(t *testing.T) {
	def := structuredAgent()
	def.Limits.ModelAttempts = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	runner := Runner{
		Agent:    def,
		Client:   client,
		Tools:    tool.List{answerLookup()},
		Recorder: NewMemoryRecorder(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "limit"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
	})
	if err != nil || !limitedForAttempts(out.Stop) {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:     "more",
		ModelAttempts: 3,
	})
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil ||
		client.sends != 2 {
		t.Fatalf("stop=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
}
