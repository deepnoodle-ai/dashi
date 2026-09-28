package dashi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type extraRunnerInput struct {
	Query string `json:"query"`
}

func extraCallResult(args string) llm.Result {
	call := tool.Call{ID: "call-1", Name: "lookup", Args: []byte(args)}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

func TestRunnerAllowsExtraTypedToolArgsBeforeHook(t *testing.T) {
	const args = `{"query":"yes","extra":1}`
	var received extraRunnerInput
	var raw tool.Call
	runs, hooks := 0, 0
	lookup, err := tool.Func(
		"lookup",
		"",
		func(
			_ context.Context,
			call tool.Call,
			in extraRunnerInput,
		) (tool.Outcome, error) {
			runs++
			raw, received = call, in
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(args),
		final("done"),
	}}
	gate := toolHook(
		"inspect",
		func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
			hooks++
			if string(plan.Call.Args) != args {
				t.Errorf("hook did not see raw arguments: %s", plan.Call.Args)
			}
			return Allow("seen"), nil
		},
	)
	out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	if client.sends != 2 || runs != 1 || hooks != 1 ||
		received.Query != "yes" {
		t.Fatalf("out=%+v err=%v sends=%d runs=%d hooks=%d input=%+v",
			out.Stop, err, client.sends, runs, hooks, received)
	}
	if string(raw.Args) != args || string(out.Tools["call-1"].Call.Args) != args {
		t.Fatalf("raw or recorded arguments changed: raw=%s recorded=%s",
			raw.Args, out.Tools["call-1"].Call.Args)
	}
}

func TestRunnerStrictTypedToolRejectsExtrasBeforeHook(t *testing.T) {
	runs, hooks := 0, 0
	lookup, err := tool.Func(
		"lookup",
		"",
		func(context.Context, tool.Call, extraRunnerInput) (tool.Outcome, error) {
			runs++
			return tool.Succeeded("found"), nil
		},
		tool.WithStrictArguments(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(`{"query":"yes","extra":1}`),
		final("done"),
	}}
	gate := toolHook(
		"inspect",
		func(context.Context, ToolPlan) (ToolDecision, error) {
			hooks++
			return Allow("seen"), nil
		},
	)
	out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	if runs != 0 || hooks != 0 ||
		out.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("out=%+v err=%v runs=%d hooks=%d", out.Stop, err, runs, hooks)
	}
	result := toolResultText(t, out.Turn, "call-1")
	if !strings.Contains(result, "Invalid tool arguments") {
		t.Fatalf("model feedback = %q", result)
	}
}

func TestRunnerRejectsAmbiguousTypedArgumentsBeforeHook(t *testing.T) {
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"query":"yes","QUERY":"evil"}`, "differs only in case"},
		{`{"QUERY":"evil","query":"yes"}`, "differs only in case"},
		{`{"query":"yes","QUERY":"no"}`, "differs only in case"},
		{`{"QUERY":"no","query":"yes"}`, "differs only in case"},
		{`{"query":"yes","query":"no"}`, "duplicate JSON object key"},
		{`{"query":"yes","extra":{"k":1,"k":2}}`, "duplicate JSON object key"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			runs, hooks := 0, 0
			lookup, err := tool.Func(
				"lookup",
				"",
				func(context.Context, tool.Call, extraRunnerInput) (tool.Outcome, error) {
					runs++
					return tool.Succeeded("found"), nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			client := &scriptedClient{results: []llm.Result{
				extraCallResult(tc.args),
				final("done"),
			}}
			gate := toolHook(
				"inspect",
				func(context.Context, ToolPlan) (ToolDecision, error) {
					hooks++
					return Allow("seen"), nil
				},
			)
			out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
			if err != nil || out.Stop.Kind != StopCompleted ||
				runs != 0 || hooks != 0 ||
				out.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
				t.Fatalf("stop=%+v err=%v runs=%d hooks=%d",
					out.Stop, err, runs, hooks)
			}
			result := toolResultText(t, out.Turn, "call-1")
			if !strings.Contains(result, tc.want) {
				t.Fatalf("model feedback = %q", result)
			}
		})
	}
}

func TestRunnerRejectsAmbiguousHookRewrite(t *testing.T) {
	for _, rewritten := range []string{
		`{"query":"yes","QUERY":"evil"}`,
		`{"query":"yes","query":"evil"}`,
	} {
		t.Run(rewritten, func(t *testing.T) {
			runs, hooks := 0, 0
			lookup, err := tool.Func(
				"lookup",
				"",
				func(context.Context, tool.Call, extraRunnerInput) (tool.Outcome, error) {
					runs++
					return tool.Succeeded("found"), nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			client := &scriptedClient{results: []llm.Result{
				extraCallResult(`{"query":"yes"}`),
				final("done"),
			}}
			gate := toolHook(
				"rewrite",
				func(context.Context, ToolPlan) (ToolDecision, error) {
					hooks++
					return Rewrite(json.RawMessage(rewritten))
				},
			)
			out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
			if err != nil || out.Stop.Kind != StopCompleted ||
				runs != 0 || hooks != 1 ||
				out.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
				t.Fatalf("stop=%+v err=%v runs=%d hooks=%d",
					out.Stop, err, runs, hooks)
			}
		})
	}
}

func TestDeferredAliasWithoutExecutorCannotRunOnLaterBinding(t *testing.T) {
	runs, hooks := 0, 0
	lookup, err := tool.Func(
		"lookup",
		"",
		func(context.Context, tool.Call, extraRunnerInput) (tool.Outcome, error) {
			runs++
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(`{"query":"yes","QUERY":"evil"}`),
		final("done"),
	}}
	gate := toolHook(
		"approve",
		func(context.Context, ToolPlan) (ToolDecision, error) {
			hooks++
			return Defer("operator approval"), nil
		},
	)
	recorder := NewMemoryRecorder()
	first, err := start(
		t,
		client,
		tool.List{{Definition: lookup.Definition}},
		recorder,
		gate.Hooks(),
	)
	if err != nil || first.Stop.Kind != StopWaiting ||
		len(first.Pending) != 1 || runs != 0 || hooks != 1 {
		t.Fatalf("first=%+v err=%v runs=%d hooks=%d",
			first.Stop, err, runs, hooks)
	}
	runner := Runner{
		Agent:    testAgent(),
		Client:   client,
		Tools:    tool.List{lookup},
		Recorder: recorder,
		Hooks:    gate.Hooks(),
	}
	approved, err := runner.Run(
		t.Context(),
		first.Turn,
		first.Pending[0].Approve(),
	)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(t.Context(), approved.Turn, Continue{CommandID: "go"})
	if err != nil || out.Stop.Kind != StopCompleted || runs != 0 || hooks != 1 ||
		out.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("out=%+v err=%v runs=%d hooks=%d",
			out.Stop, err, runs, hooks)
	}
}

func TestRunnerRevalidatesRewrittenExtraArguments(t *testing.T) {
	runs := 0
	var received extraRunnerInput
	var raw tool.Call
	lookup, err := tool.Func(
		"lookup",
		"",
		func(
			_ context.Context,
			call tool.Call,
			in extraRunnerInput,
		) (tool.Outcome, error) {
			runs++
			raw, received = call, in
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	const requested = `{"query":"yes","extra":1}`
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(requested),
		final("done"),
	}}
	gate := toolHook(
		"normalize",
		func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
			if len(plan.Call.RequestedArgs) > 0 {
				return ToolDecision{}, nil
			}
			return Rewrite(map[string]string{"query": "updated"})
		},
	)
	out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	if runs != 1 || received.Query != "updated" {
		t.Fatalf("out=%+v err=%v runs=%d input=%+v", out.Stop, err, runs, received)
	}
	if !json.Valid(raw.Args) || string(raw.RequestedArgs) != requested ||
		string(raw.Args) != `{"query":"updated"}` {
		t.Fatalf("effective=%s requested=%s", raw.Args, raw.RequestedArgs)
	}
}
