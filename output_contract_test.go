package dashi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

var answerSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`)

func structuredAgent() Agent {
	d := testAgent()
	d.OutputSchema = append(json.RawMessage(nil), answerSchema...)
	return d
}

func submitResult(id string, args string) llm.Result {
	call := tool.Call{ID: id, Name: SubmitOutputTool, Args: json.RawMessage(args)}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

func applyStructured(
	t *testing.T,
	client *scriptedClient,
	tools tool.Toolset,
	rec Recorder,
) RunResult {
	t.Helper()
	out, err := applyStructuredErr(client, tools, rec)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func applyStructuredErr(
	client *scriptedClient,
	tools tool.Toolset,
	rec Recorder,
) (RunResult, error) {
	return (Runner{
		Client:   client,
		Tools:    tools,
		Recorder: rec,
	}).Run(
		context.Background(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     structuredAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
}

func TestStructuredOutputAfterOrdinaryTool(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{
			callResult("work", "lookup"),
			submitResult("submit", `{"answer":42}`),
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
				Output: new(tool.TextOutput("found 42")),
			}, nil
		}),
	}
	rec := NewMemoryRecorder()
	out := applyStructured(t, client, tool.List{binding}, rec)
	if out.Stop.Kind != StopCompleted || out.Output == nil || string(out.Output.Value) != `{"answer":42}` ||
		out.Output.Source != OutputFromTool || out.Output.ToolCallID != "submit" || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v sends=%d runs=%d", out, client.sends, runs)
	}
	for i, in := range client.inputs {
		if len(in.OutputSchema) != 0 {
			t.Fatalf("request %d leaked native output schema into turn", i)
		}
		if len(in.Tools) != 2 || in.Tools[1].Name != SubmitOutputTool || string(in.Tools[1].Schema) != string(answerSchema) {
			t.Fatalf("request %d tools=%+v", i, in.Tools)
		}
	}
	view, err := out.Turn.View()
	if err != nil || view.Output == nil || view.Output.Source != OutputFromTool {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	view.Output.Value[0] = '['
	if string(out.Output.Value) != `{"answer":42}` {
		t.Fatal("structured output shares mutable bytes")
	}
	again, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		out.Turn,
		Start{
			CommandID: "start",
			Agent:     structuredAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil || again.Stop.Kind != StopCompleted || client.sends != 2 || runs != 1 {
		t.Fatalf(
			"duplicate start=%+v err=%v sends=%d runs=%d",
			again,
			err,
			client.sends,
			runs,
		)
	}
}

func TestStructuredTurnAfterModelHookUsesSubmissionTool(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submit", `{"answer":42}`)},
	}
	hooks := Hooks{BeforeModel: []ModelHook{{
		Name: "settings",
		Func: func(_ context.Context, plan ModelPlan) (ModelDecision, error) {
			settings := plan.Input.Settings
			settings.MaxTokens = 256
			return ModelDecision{Settings: &settings}, nil
		},
	}}}
	out, err := (Runner{
		Client: client,
		Hooks:  hooks,
	}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     structuredAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil ||
		out.Output.Source != OutputFromTool || out.Output.ToolCallID != "submit" {
		t.Fatalf("turn did not use submission tool: output=%+v err=%v", out, err)
	}
	if len(client.inputs) != 1 || len(client.inputs[0].OutputSchema) != 0 ||
		client.inputs[0].Settings.MaxTokens != 256 ||
		len(client.inputs[0].Tools) != 1 ||
		client.inputs[0].Tools[0].Name != SubmitOutputTool {
		t.Fatalf("turn sent native output or lost submission tool: %+v", client.inputs)
	}
}

func TestStructuredOutputRejectsBadSubmissionThenAcceptsCorrection(
	t *testing.T,
) {
	client := &scriptedClient{
		results: []llm.Result{
			submitResult("bad", `{"answer":"wrong"}`),
			submitResult("good", `{"answer":7}`),
		},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	if out.Stop.Kind != StopCompleted || out.Output == nil || out.Output.ToolCallID != "good" || client.sends != 2 {
		t.Fatalf("out=%+v sends=%d", out, client.sends)
	}
	if len(client.inputs[1].Messages) != 3 {
		t.Fatalf("messages=%d", len(client.inputs[1].Messages))
	}
	feedback := client.inputs[1].Messages[2].Content[0].ToolResult
	if feedback == nil || !feedback.IsError || !strings.Contains(
		feedback.Output.Content[0].Text.Text,
		"Output rejected",
	) {
		t.Fatalf("feedback=%+v", feedback)
	}
}

func TestStructuredOutputTextFallbackAndUnsatisfiedFailure(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		stop       StopKind
	}{
		{"plain", `{"answer":3}`, StopCompleted},
		{"fenced", "```json\n{\"answer\":3}\n```", StopCompleted},
		{"invalid", `{"answer":"three"}`, StopFailed},
		{"prose", `The answer is 3.`, StopFailed},
		{"empty", ``, StopFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedClient{results: []llm.Result{final(tc.text)}}
			out := applyStructured(t, client, nil, NewMemoryRecorder())
			if out.Stop.Kind != tc.stop {
				t.Fatalf("stop=%+v", out.Stop)
			}
			if tc.stop == StopCompleted {
				if out.Output == nil || out.Output.Source != OutputFromText || string(out.Output.Value) != `{"answer":3}` {
					t.Fatalf("output=%+v", out.Output)
				}
			} else if out.Output != nil || !strings.Contains(
				out.Stop.Reason,
				"output_schema_unsatisfied",
			) {
				t.Fatalf("invalid final accepted: %+v", out)
			}
		})
	}
}

func TestStructuredOutputStartedSubmissionRecoversWithoutModelResend(
	t *testing.T,
) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepToolFinished}
	first, err := applyStructuredErr(client, nil, rec)
	if !nothingWritten(err) || client.sends != 1 {
		t.Fatalf("first=%+v sends=%d", first, client.sends)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := saved.View()
	if err != nil || view.Tools["submission"].Status != ToolStarted {
		t.Fatalf("saved=%+v err=%v", view, err)
	}
	second, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), saved, Continue{CommandID: "recover"})
	if err != nil || second.Stop.Kind != StopCompleted || second.Output == nil || client.sends != 1 {
		t.Fatalf("recovered=%+v err=%v sends=%d", second, err, client.sends)
	}
}

func TestStructuredOutputRecordRejectsForgedValue(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	for i := range out.Turn.Steps {
		if out.Turn.Steps[i].Kind == StepOutputRecorded {
			out.Turn.Steps[i].Output.Value = json.RawMessage(`{"answer":9}`)
			break
		}
	}
	if _, err := out.Turn.View(); err == nil {
		t.Fatal("replay accepted output that did not match the recorded model call")
	}
}

func TestStructuredOutputSchemaCanVaryPerTurn(t *testing.T) {
	const schema = `{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`
	def := structuredAgent()
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"label":"ready"}`)},
	}
	start := Start{
		CommandID:    "start",
		Agent:        def,
		Input:        []llm.Message{llm.UserMessage(llm.Text("status"))},
		OutputSchema: json.RawMessage(schema),
	}
	out, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(t.Context(), Turn{ID: "t1"}, start)
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil || string(out.Output.Value) != `{"label":"ready"}` {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(client.inputs) != 1 || string(client.inputs[0].Tools[0].Schema) != schema {
		t.Fatalf("submitted schema=%+v", client.inputs)
	}
	view, err := out.Turn.View()
	if err != nil || string(view.OutputSchema) != schema || string(view.Agent.OutputSchema) != string(answerSchema) {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	// The admitted request is part of the record; altering it invalidates the
	// accepted model intent and submission evidence.
	for i := range out.Turn.Steps {
		if out.Turn.Steps[i].Kind == StepTurnStarted {
			out.Turn.Steps[i].Started.OutputSchema = append(
				json.RawMessage(nil),
				answerSchema...,
			)
			break
		}
	}
	if _, err := out.Turn.View(); err == nil {
		t.Fatal("replay accepted a changed turn schema")
	}
}

func TestStructuredOutputStartRejectsInvalidTurnSchema(t *testing.T) {
	for _, schema := range []string{
		`{"type":"array"}`,
		`{"type":"object","$ref":"https://example.com/external"}`,
		`{`,
	} {
		client := &scriptedClient{}
		out, err := (Runner{
			Client:   client,
			Recorder: NewMemoryRecorder(),
		}).Run(
			t.Context(),
			Turn{ID: "t1"},
			Start{
				CommandID:    "start",
				Agent:        testAgent(),
				Input:        []llm.Message{llm.UserMessage(llm.Text("answer"))},
				OutputSchema: json.RawMessage(schema),
			},
		)
		if err == nil || len(out.Turn.Steps) != 0 || client.sends != 0 {
			t.Fatalf("schema=%s out=%+v err=%v", schema, out, err)
		}
	}
}

func TestSameJSONPreservesLargeIntegerIdentity(t *testing.T) {
	if canonjson.Equal(
		json.RawMessage(`{"id":9007199254740992}`),
		json.RawMessage(`{"id":9007199254740993}`),
	) {
		t.Fatal("distinct integers compared equal")
	}
}

func TestStructuredOutputSubmissionEvidenceCannotBeRewritten(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	for i := range out.Turn.Steps {
		if out.Turn.Steps[i].Kind == StepToolDecision {
			out.Turn.Steps[i].Tool.Call.Args = json.RawMessage(`{"answer":9}`)
			out.Turn.Steps[i].Tool.Call.RequestedArgs = json.RawMessage(`{"answer":8}`)
			break
		}
	}
	if _, err := out.Turn.View(); err == nil {
		t.Fatal("replay accepted a rewritten output submission")
	}
}

func TestStructuredOutputCannotBeSettledByHost(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepToolFinished}
	first, err := applyStructuredErr(client, nil, rec)
	if !nothingWritten(err) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	result := tool.Outcome{State: tool.StateSucceeded}
	_, err = (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		saved,
		Resolve{CommandID: "host-result", CallID: "submission", Outcome: &result},
	)
	if err == nil {
		t.Fatal("host settled a runner-owned output submission")
	}
}

func TestStructuredOutputRecordedOutputRejectsContextAndCancelUntilCompleted(
	t *testing.T,
) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepTurnStopped}
	first, err := applyStructuredErr(client, nil, rec)
	if !nothingWritten(err) {
		t.Fatalf("err=%v", err)
	}
	if first.Output == nil {
		t.Fatal("first outcome has no output")
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d", client.sends)
	}

	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := saved.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Output == nil {
		t.Fatal("saved record has no output")
	}
	if view.Stop.Kind != "" {
		t.Fatalf("stop=%q", view.Stop.Kind)
	}

	runner := Runner{
		Client:   client,
		Recorder: rec,
	}
	deliver := AddMessages{
		CommandID: "nudge",
		Items: []ContextItem{
			{Origin: "user", Message: llm.UserMessage(llm.Text("more"))},
		},
	}
	if _, err := runner.Run(
		t.Context(),
		saved,
		deliver,
	); err == nil {
		t.Fatal("deliver accepted after recorded output")
	}
	assertStepCount(t, base, len(saved.Steps))

	if _, err := runner.Run(
		t.Context(),
		saved,
		Cancel{CommandID: "cancel"},
	); err == nil {
		t.Fatal("cancel accepted after recorded output")
	}
	assertStepCount(t, base, len(saved.Steps))

	out, err := runner.Run(
		t.Context(),
		saved,
		Continue{CommandID: "recover"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%q", out.Stop.Kind)
	}
	if out.Output == nil || string(out.Output.Value) != `{"answer":8}` {
		t.Fatalf("output=%+v", out.Output)
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d", client.sends)
	}
	if len(out.Turn.Steps) != len(saved.Steps)+2 {
		t.Fatalf("steps=%d saved=%d", len(out.Turn.Steps), len(saved.Steps))
	}
}

func assertStepCount(t *testing.T, base *MemoryRecorder, want int) {
	t.Helper()
	turn, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Steps) != want {
		t.Fatalf("steps=%d want=%d", len(turn.Steps), want)
	}
}

func TestReplayRejectsContextAfterRecordedOutput(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	i := -1
	for j, step := range out.Turn.Steps {
		if step.Kind == StepOutputRecorded {
			i = j
			break
		}
	}
	if i < 0 {
		t.Fatal("no recorded output")
	}
	recorded := out.Turn.Steps[i]
	seq := int64(i + 2)

	forged := []Step{
		{
			Version: recorded.Version,
			TurnID:  recorded.TurnID,
			Seq:     seq,
			Kind:    StepMessagesAdded,
			Context: &ContextFact{
				Items: []ContextItem{
					{Origin: "user", Message: llm.UserMessage(llm.Text("more"))},
				},
			},
		},
		{
			Version: recorded.Version,
			TurnID:  recorded.TurnID,
			Seq:     seq,
			Kind:    StepTurnStopped,
			Stop:    testStop(StopCanceled, ""),
		},
	}
	for _, step := range forged {
		turn := out.Turn
		turn.Steps = append(slices.Clone(out.Turn.Steps[:i+1]), step)
		if _, err := turn.View(); err == nil {
			t.Fatalf("replay accepted %s after recorded output", step.Kind)
		}
	}
}
