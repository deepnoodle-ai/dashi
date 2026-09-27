package kernel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestReservedFinalAnswerUsesLastAttemptWithoutExtraCall(t *testing.T) {
	def := definition()
	def.Limits = DefaultLimits()
	def.Limits.ModelAttempts = 2
	def.Settings.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAny}
	client := &scriptedClient{results: []llm.Result{callResult("lookup-1", "lookup"), final("Found 42; further work is unfinished.")}}
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("42"))}, nil
		})}
	rec := NewMemoryRecorder()
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "limit"}, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("look up 42"))}},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil || out.Stop != (Stop{Kind: StopLimited, Reason: "model_attempts"}) ||
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
	again, err := (Engine{}).Apply(context.Background(), out.Turn, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("look up 42"))}},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil || again.Stop.Kind != StopLimited || client.sends != 2 || runs != 1 {
		t.Fatalf("duplicate start=%+v err=%v sends=%d runs=%d", again, err, client.sends, runs)
	}
	if _, err := (Engine{}).Apply(context.Background(), out.Turn, def, Continue{ID: "too-late"},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}}); err == nil {
		t.Fatal("exhausted turn accepted a continuation")
	}
}

func TestReservedFinalAnswerKeepsStructuredOutputSubmission(t *testing.T) {
	def := structuredDefinition()
	def.Limits.ModelAttempts = 2
	def.Limits.ReserveFinalAnswer = true
	client := &scriptedClient{results: []llm.Result{callResult("lookup-1", "lookup"), submitResult("submit", `{"answer":42}`)}}
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("42"))}, nil
		})}
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "limit"}, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("look up 42"))}},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: NewMemoryRecorder(), Policy: AllowLocalPolicy{}})
	if err != nil || out.Stop != (Stop{Kind: StopLimited, Reason: "model_attempts"}) ||
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

func TestReservedFinalAnswerCannotExecuteUnadvertisedTool(t *testing.T) {
	def := definition()
	def.Limits.ModelAttempts = 2
	def.Limits.ReserveFinalAnswer = true
	client := &scriptedClient{results: []llm.Result{callResult("first", "lookup"), callResult("second", "lookup")}}
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("found"))}, nil
		})}
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "limit"}, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("look up"))}},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: NewMemoryRecorder(), Policy: AllowLocalPolicy{}})
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
	def := definition()
	def.Limits.ModelAttempts = 2
	def.Limits.ReserveFinalAnswer = true
	client := &scriptedClient{results: []llm.Result{callResult("first", "lookup"), final("42")}}
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("42"))}, nil
		})}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepStopped}
	first, err := (Engine{}).Apply(context.Background(), Turn{ID: "limit"}, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("look up"))}},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil || first.Persistence != PersistenceRejected || client.sends != 2 {
		t.Fatalf("first=%+v err=%v sends=%d", first, err, client.sends)
	}
	saved, err := base.Load("limit")
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Engine{}).Apply(context.Background(), saved, def, Continue{ID: "recover"},
		Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil || second.Stop != (Stop{Kind: StopLimited, Reason: "model_attempts"}) ||
		second.Turn.AnswerText() != "42" || client.sends != 2 {
		t.Fatalf("recovered=%+v err=%v sends=%d", second, err, client.sends)
	}
}
