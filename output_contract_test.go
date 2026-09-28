package dashi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

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
) Outcome {
	t.Helper()
	out, err := (Runner{
		Agent:    structuredAgent(),
		Client:   client,
		Tools:    tools,
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
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
	out := applyStructured(t, client, fixedTools{binding}, rec)
	if out.Stop.Kind != StopCompleted || out.Output == nil || string(out.Output.Value) != `{"answer":42}` ||
		out.Output.Source != OutputFromTool || out.Output.ToolCallID != "submit" || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v sends=%d runs=%d", out, client.sends, runs)
	}
	for i, in := range client.inputs {
		if len(in.Tools) != 2 || in.Tools[1].Name != SubmitOutputTool || string(in.Tools[1].Schema) != string(answerSchema) {
			t.Fatalf("request %d tools=%+v", i, in.Tools)
		}
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Output == nil || view.Output.Source != OutputFromTool {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
	result, ok, err := out.Turn.StructuredOutput()
	if err != nil || !ok || string(result.Value) != `{"answer":42}` {
		t.Fatalf("turn output=%+v ok=%t err=%v", result, ok, err)
	}
	result.Value[0] = '['
	if string(out.Output.Value) != `{"answer":42}` {
		t.Fatal("structured output shares mutable bytes")
	}
	again, err := (Runner{
		Agent:    structuredAgent(),
		Client:   client,
		Tools:    fixedTools{binding},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}).Run(
		t.Context(),
		out.Turn,
		Start{
			CommandID: "start",
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
	rec := &failOnceKind{base: base, kind: StepToolSettled}
	first := applyStructured(t, client, nil, rec)
	if first.Persistence != PersistenceRejected || client.sends != 1 {
		t.Fatalf("first=%+v sends=%d", first, client.sends)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(saved)
	if err != nil || view.Tools["submission"].Status != ToolStarted {
		t.Fatalf("saved=%+v err=%v", view, err)
	}
	second, err := (Runner{
		Agent:    structuredAgent(),
		Client:   client,
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
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
	if _, err := Replay(out.Turn); err == nil {
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
		Input:        []llm.Message{llm.UserMessage(llm.Text("status"))},
		OutputSchema: json.RawMessage(schema),
	}
	out, err := (Runner{
		Agent:    def,
		Client:   client,
		Recorder: NewMemoryRecorder(),
		Policy:   AllowLocalPolicy{},
	}).Run(t.Context(), Turn{ID: "t1"}, start)
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil || string(out.Output.Value) != `{"label":"ready"}` {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(client.inputs) != 1 || string(client.inputs[0].Tools[0].Schema) != schema {
		t.Fatalf("submitted schema=%+v", client.inputs)
	}
	view, err := Replay(out.Turn)
	if err != nil || string(view.OutputSchema) != schema || string(view.Agent.OutputSchema) != string(answerSchema) {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	// The admitted request is part of the record; altering it invalidates the
	// accepted model intent and submission evidence.
	for i := range out.Turn.Steps {
		if out.Turn.Steps[i].Kind == StepStarted {
			out.Turn.Steps[i].Started.OutputSchema = append(
				json.RawMessage(nil),
				answerSchema...,
			)
			break
		}
	}
	if _, err := Replay(out.Turn); err == nil {
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
			Agent:    testAgent(),
			Client:   client,
			Recorder: NewMemoryRecorder(),
			Policy:   AllowLocalPolicy{},
		}).Run(
			t.Context(),
			Turn{ID: "t1"},
			Start{
				CommandID:    "start",
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
	if sameJSON(
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
	if _, err := Replay(out.Turn); err == nil {
		t.Fatal("replay accepted a rewritten output submission")
	}
}

func TestStructuredOutputCannotBeSettledByHost(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepToolSettled}
	first := applyStructured(t, client, nil, rec)
	if first.Persistence != PersistenceRejected {
		t.Fatalf("first=%+v", first)
	}
	saved, err := base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	result := tool.Outcome{State: tool.StateSucceeded}
	_, err = (Runner{
		Agent:    structuredAgent(),
		Client:   client,
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}).Run(
		t.Context(),
		saved,
		Settle{CommandID: "host-result", CallID: "submission", Outcome: &result},
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
	rec := &failOnceKind{base: base, kind: StepStopped}
	first := applyStructured(t, client, nil, rec)
	if first.Persistence != PersistenceRejected {
		t.Fatalf("persistence=%q", first.Persistence)
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
	view, err := Replay(saved)
	if err != nil {
		t.Fatal(err)
	}
	if view.Output == nil {
		t.Fatal("saved record has no output")
	}
	if view.Stop.Kind != "" {
		t.Fatalf("stop=%q", view.Stop.Kind)
	}

	def := structuredAgent()
	runner := Runner{
		Agent:    def,
		Client:   client,
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
	}
	deliver := Deliver{
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
			Kind:    StepDelivered,
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
			Kind:    StepStopped,
			Stop:    &Stop{Kind: StopCanceled},
		},
	}
	for _, step := range forged {
		turn := out.Turn
		turn.Steps = append(slices.Clone(out.Turn.Steps[:i+1]), step)
		if _, err := Replay(turn); err == nil {
			t.Fatalf("replay accepted %s after recorded output", step.Kind)
		}
	}
}
