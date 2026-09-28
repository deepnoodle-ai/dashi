package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type extraRunnerInput struct {
	Query string `json:"query"`
}

type duplicateRunnerChild struct {
	Label  string `json:"label"`
	Region string `json:"region,omitempty" enum:"us,eu"`
}

type duplicateRunnerInput struct {
	Query string               `json:"query" enum:"yes,no"`
	Child duplicateRunnerChild `json:"child"`
}

type guardedToolset struct {
	tool     tool.Tool
	calls    int
	executed *bool
}

type changingToolset struct {
	tool  tool.Tool
	calls int
	mode  string
}

func (s *changingToolset) Tools(context.Context) ([]tool.Tool, error) {
	s.calls++
	if s.calls == 2 {
		switch s.mode {
		case "error":
			return nil, errors.New("prehook lookup failed")
		case "missing":
			return nil, nil
		}
	}
	return []tool.Tool{s.tool}, nil
}

func (s *guardedToolset) Tools(context.Context) ([]tool.Tool, error) {
	s.calls++
	if s.calls > 2 && !*s.executed {
		return nil, errors.New("third lookup before execution")
	}
	return []tool.Tool{s.tool}, nil
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

func TestRunnerRejectsCaseAliasesBeforeHook(t *testing.T) {
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"query":"yes","QUERY":"evil"}`, "differs only in case"},
		{`{"QUERY":"evil","query":"yes"}`, "differs only in case"},
		{`{"query":"yes","QUERY":"no"}`, "differs only in case"},
		{`{"QUERY":"no","query":"yes"}`, "differs only in case"},
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

func TestRunnerRejectsCaseAliasHookRewrite(t *testing.T) {
	for _, rewritten := range []string{
		`{"query":"yes","QUERY":"evil"}`,
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

func TestRunnerDuplicateKeysAgreeWithNormalizedReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want duplicateRunnerInput
	}{
		{
			name: "root",
			raw: `{"query":"evil","query":"yes",` +
				`"child":{"label":"a","region":"us"}}`,
			want: duplicateRunnerInput{
				Query: "yes",
				Child: duplicateRunnerChild{Label: "a", Region: "us"},
			},
		},
		{
			name: "nested",
			raw: `{"query":"yes",` +
				`"child":{"label":"a","region":"evil"},` +
				`"child":{"label":"b","region":"us"}}`,
			want: duplicateRunnerInput{
				Query: "yes",
				Child: duplicateRunnerChild{Label: "b", Region: "us"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var meaning any
			if err := json.Unmarshal([]byte(tc.raw), &meaning); err != nil {
				t.Fatal(err)
			}
			replayed, err := json.Marshal(meaning)
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range []string{tc.raw, string(replayed)} {
				runs, hooks := 0, 0
				var received duplicateRunnerInput
				lookup, err := tool.Func(
					"lookup",
					"",
					func(
						_ context.Context,
						_ tool.Call,
						in duplicateRunnerInput,
					) (tool.Outcome, error) {
						runs++
						received = in
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
							t.Errorf("hook raw arguments = %s", plan.Call.Args)
						}
						return Allow("seen"), nil
					},
				)
				out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
				if err != nil || out.Stop.Kind != StopCompleted ||
					runs != 1 || hooks != 1 || received != tc.want {
					t.Fatalf("args=%s stop=%+v err=%v runs=%d hooks=%d received=%+v",
						args, out.Stop, err, runs, hooks, received)
				}
			}
		})
	}
}

func TestRunnerReusesPrehookTypedToolBinding(t *testing.T) {
	executed := false
	var set *guardedToolset
	lookup, err := tool.Func(
		"lookup",
		"",
		func(context.Context, tool.Call, extraRunnerInput) (tool.Outcome, error) {
			if set.calls != 2 {
				t.Errorf("toolset calls before callback = %d, want 2", set.calls)
			}
			executed = true
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	set = &guardedToolset{tool: lookup, executed: &executed}
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(`{"query":"yes"}`),
		final("done"),
	}}
	gate := toolHook(
		"inspect",
		func(context.Context, ToolPlan) (ToolDecision, error) {
			return Allow("seen"), nil
		},
	)
	out, err := runHooked(t, client, set, gate.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || !executed {
		t.Fatalf("stop=%+v err=%v calls=%d executed=%v",
			out.Stop, err, set.calls, executed)
	}
}

func TestRunnerPrehookBindingFailureCannotExecuteLaterTool(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		wantHooks  int
		wantReason string
	}{
		{
			mode:       "error",
			wantHooks:  0,
			wantReason: "prehook lookup failed",
		},
		{
			mode:       "missing",
			wantHooks:  1,
			wantReason: "tool binding changed or unavailable",
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
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
			set := &changingToolset{tool: lookup, mode: tc.mode}
			client := &scriptedClient{results: []llm.Result{
				extraCallResult(`{"query":"yes","QUERY":"evil"}`),
				final("done"),
			}}
			gate := toolHook(
				"inspect",
				func(context.Context, ToolPlan) (ToolDecision, error) {
					hooks++
					return Allow("seen"), nil
				},
			)
			out, err := runHooked(t, client, set, gate.Hooks())
			if err != nil || out.Stop.Kind != StopFailed ||
				!strings.Contains(out.Stop.Reason, tc.wantReason) ||
				runs != 0 || hooks != tc.wantHooks || set.calls != 2 {
				t.Fatalf("stop=%+v err=%v runs=%d hooks=%d lookups=%d",
					out.Stop, err, runs, hooks, set.calls)
			}
		})
	}
}

func TestRunnerDuplicateKeyHookRewriteUsesLastValue(t *testing.T) {
	const requested = `{"query":"no"}`
	const rewritten = `{"query":"evil","query":"yes"}`
	var received extraRunnerInput
	var callbackCall tool.Call
	lookup, err := tool.Func(
		"lookup",
		"",
		func(
			_ context.Context,
			call tool.Call,
			in extraRunnerInput,
		) (tool.Outcome, error) {
			received, callbackCall = in, call
			return tool.Succeeded("found"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{
		extraCallResult(requested),
		final("done"),
	}}
	gate := toolHook(
		"rewrite",
		func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
			if len(plan.Call.RequestedArgs) != 0 {
				return ToolDecision{}, nil
			}
			return Rewrite(json.RawMessage(rewritten))
		},
	)
	out, err := runHooked(t, client, tool.List{lookup}, gate.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted ||
		received.Query != "yes" ||
		string(callbackCall.Args) != rewritten ||
		string(callbackCall.RequestedArgs) != requested {
		t.Fatalf("stop=%+v err=%v input=%+v callback=%+v",
			out.Stop, err, received, callbackCall)
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
