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
		Policy:   AllowLocalPolicy{},
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || !limitedForAttempts(out.Stop) ||
		out.Turn.AnswerText() != "Found 42; further work is unfinished." || client.sends != 2 || runs != 1 {
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
		Policy:   AllowLocalPolicy{},
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
		Policy:   AllowLocalPolicy{},
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
		Policy:   AllowLocalPolicy{},
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
	value, ok, err := out.Turn.StructuredOutput()
	if err != nil || !ok || !json.Valid(value.Value) {
		t.Fatalf("structured output=%+v ok=%t err=%v", value, ok, err)
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
			stop: Stop{Kind: StopLimited, Reason: "model_attempts"},
		},
		{
			name:    "deadline",
			stop:    Stop{Kind: StopLimited, Reason: "deadline"},
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
		Policy:   AllowLocalPolicy{},
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
		Policy:   AllowLocalPolicy{},
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
		Policy:   AllowLocalPolicy{},
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up"))},
		},
	)
	if err != nil || first.Persistence != PersistenceRejected || client.sends != 2 {
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
		Policy:   AllowLocalPolicy{},
	}).Run(t.Context(), saved, Continue{CommandID: "recover"})
	if err != nil || !limitedForAttempts(second.Stop) ||
		second.Turn.AnswerText() != "42" || client.sends != 2 {
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
