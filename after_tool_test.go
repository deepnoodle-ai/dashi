package dashi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// resultText joins the text blocks of a tool result.
func modelText(r llm.ToolResult) string {
	var b strings.Builder
	for _, block := range r.Output.Content {
		if block.Text != nil {
			b.WriteString(block.Text.Text)
		}
	}
	return b.String()
}

func replaceText(name string, f func(string) string) ResultHook {
	return ResultHook{Name: name, Func: func(
		_ context.Context,
		p ResultPlan,
	) (ResultDecision, error) {
		r := p.Result
		r.Output = r.Output.MapText(f)
		return ResultDecision{Replace: &r}, nil
	}}
}

// secretTool returns a result with a secret in it.
func secretTool(runs *int) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "remote",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			*runs++
			return tool.Succeeded("key=sk-123 ok"), nil
		}),
	}
}

func TestAfterToolChainsReplacements(t *testing.T) {
	var seen []string
	first := replaceText("redact", func(s string) string {
		seen = append(seen, s)
		return strings.ReplaceAll(s, "sk-123", "[redacted]")
	})
	second := replaceText("shout", func(s string) string {
		seen = append(seen, s)
		return strings.ToUpper(s)
	})
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(
		t,
		client,
		tool.List{secretTool(&runs)},
		Hooks{AfterTool: []ResultHook{first, second}},
	)
	if err != nil || out.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
	}
	if strings.Join(seen, "|") != "key=sk-123 ok|key=[redacted] ok" {
		t.Fatalf("hooks saw %q", seen)
	}
	view := out.Tools["call-1"]
	if got := modelText(view.Result()); got != "KEY=[REDACTED] OK" {
		t.Fatalf("model result = %q", got)
	}
	if got := modelText(modelToolResult("call-1", view.Outcome)); got != "key=sk-123 ok" {
		t.Fatalf("the outcome changed: %q", got)
	}
	if seen := toolResultText(t, out.Turn, "call-1"); seen != "KEY=[REDACTED] OK" {
		t.Fatalf("projection = %q", seen)
	}
	sent := client.inputs[1].Messages
	if !strings.Contains(sent[len(sent)-1].Content[0].ToolResult.Output.Content[0].Text.Text, "REDACTED") {
		t.Fatalf("the model saw the raw result")
	}
	settled := settlementOf(t, out.Turn, "call-1")
	if settled.Tool.Result == nil || len(settled.HookResults) != 2 ||
		settled.HookResults[0].Verdict != VerdictReplace {
		t.Fatalf("settled = %+v", settled)
	}
	if _, err := Replay(out.Turn); err != nil {
		t.Fatal(err)
	}
}

func TestAfterToolEqualReplacementIsNoOpinion(t *testing.T) {
	same := replaceText("noop", func(s string) string { return s })
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{secretTool(&runs)}, same.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	settled := settlementOf(t, out.Turn, "call-1")
	if settled.Tool.Result != nil || len(settled.HookResults) != 0 {
		t.Fatalf("recorded a no-op replacement: %+v", settled)
	}
}

func TestAfterToolFailsClosed(t *testing.T) {
	later := 0
	after := replaceText("later", func(s string) string {
		later++
		return s
	})
	for name, h := range map[string]ResultHook{
		"error": {Name: "check", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
			return ResultDecision{}, errors.New("scanner down")
		}},
		"panic": {Name: "check", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
			panic("scanner crashed")
		}},
		"invalid replacement": {Name: "check", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
			return ResultDecision{Replace: &llm.ToolResult{
				Output: tool.Output{Content: []content.Block{{Kind: "bogus"}}},
			}}, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			later = 0
			runs := 0
			client := &scriptedClient{
				results: []llm.Result{callResult("call-1", "remote"), final("done")},
			}
			out, err := runHooked(
				t,
				client,
				tool.List{secretTool(&runs)},
				Hooks{AfterTool: []ResultHook{h, after}},
			)
			if err != nil || out.Stop.Kind != StopCompleted || runs != 1 {
				t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
			}
			result := out.Tools["call-1"].Result()
			want := "The tool ran, but its result was withheld: check check failed."
			if modelText(result) != want || result.IsError {
				t.Fatalf("model result = %+v", result)
			}
			if later != 0 || !hasWarning(out, WarningHookFailed) {
				t.Fatalf("later=%d warnings=%+v", later, out.Warnings)
			}
			if out.Tools["call-1"].Outcome.State != tool.StateSucceeded {
				t.Fatalf("outcome = %+v", out.Tools["call-1"].Outcome)
			}
		})
	}
}

func TestAfterToolFailOpenKeepsTheResult(t *testing.T) {
	flaky := ResultHook{Name: "flaky", FailOpen: true, Func: func(context.Context, ResultPlan) (ResultDecision, error) {
		return ResultDecision{}, errors.New("cosmetic failure")
	}}
	redact := replaceText("redact", func(s string) string {
		return strings.ReplaceAll(s, "sk-123", "[redacted]")
	})
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(
		t,
		client,
		tool.List{secretTool(&runs)},
		Hooks{AfterTool: []ResultHook{flaky, redact}},
	)
	if err != nil || out.Stop.Kind != StopCompleted ||
		modelText(out.Tools["call-1"].Result()) != "key=[redacted] ok" ||
		!hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v result=%+v warnings=%+v err=%v", out.Stop, out.Tools["call-1"].Result(), out.Warnings, err)
	}
}

func TestAfterToolWithholdsWhenTheRunEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	slow := ResultHook{Name: "slow", Func: func(ctx context.Context, _ ResultPlan) (ResultDecision, error) {
		cancel()
		<-ctx.Done()
		return ResultDecision{}, ctx.Err()
	}}
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := (Runner{
		Agent:  testAgent(),
		Client: client,
		Tools:  tool.List{secretTool(&runs)},
		Hooks:  slow.Hooks(),
	}).Run(ctx, Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopInterrupted || runs != 1 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
	}
	view := out.Tools["call-1"]
	if view.Status != ToolSettled || view.Outcome.State != tool.StateSucceeded ||
		!strings.Contains(modelText(view.Result()), "withheld") ||
		!hasWarning(out, WarningHookFailed) {
		t.Fatalf("view=%+v result=%+v warnings=%+v", view, view.Result(), out.Warnings)
	}
	if _, err := Project(out.Turn); err != nil {
		t.Fatalf("project: %v", err)
	}
}

func TestAfterToolRunsOneResultAtATime(t *testing.T) {
	calls := 0 // unguarded: the race detector flags concurrent hook calls
	count := ResultHook{Name: "count", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
		calls++
		return ResultDecision{}, nil
	}}
	both := llm.Result{
		Message: llm.AssistantMessage(
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "a", Name: "remote", Args: []byte(`{}`)}},
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "b", Name: "remote", Args: []byte(`{}`)}},
		),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	// The executors run in parallel, so they count nothing.
	parallel := tool.Tool{
		Definition: tool.Definition{
			Name:   "remote",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		}),
	}
	client := &scriptedClient{results: []llm.Result{both, final("done")}}
	out, err := runHooked(t, client, tool.List{parallel}, count.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || calls != 2 {
		t.Fatalf("stop=%+v calls=%d err=%v", out.Stop, calls, err)
	}
}

func TestAfterToolRunsOnlyForToolAndHostContent(t *testing.T) {
	called := map[string]int{}
	watch := ResultHook{Name: "watch", Func: func(_ context.Context, p ResultPlan) (ResultDecision, error) {
		called[p.Call.ID]++
		return ResultDecision{}, nil
	}}
	deny := ToolHook{Name: "deny", Func: func(_ context.Context, p ToolPlan) (ToolDecision, error) {
		if p.Call.ID == "denied" {
			return Deny("no"), nil
		}
		return ToolDecision{}, nil
	}}
	notRun := tool.Tool{
		Definition: tool.Definition{
			Name:   "skip",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateNotExecuted,
				Output: new(tool.TextOutput("skipped by the tool")),
			}, nil
		}),
	}
	failing := tool.Tool{
		Definition: tool.Definition{
			Name:   "broken",
			Schema: []byte(`{"type":"object","required":["x"]}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{}, errors.New("tool error")
		}),
	}
	call := func(id, name, args string) llm.Content {
		return llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: id, Name: name, Args: []byte(args)}}
	}
	batch := llm.Result{
		Message: llm.AssistantMessage(
			call("denied", "skip", `{}`),
			call("invalid", "broken", `{}`),
			call("unknown", "nope", `{}`),
			call("not-run", "skip", `{}`),
			call("failed", "broken", `{"x":1}`),
		),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	client := &scriptedClient{results: []llm.Result{batch, final("done")}}
	out, err := runHooked(
		t,
		client,
		tool.List{notRun, failing},
		Hooks{BeforeTool: []ToolHook{deny}, AfterTool: []ResultHook{watch}},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if called["not-run"] != 1 || called["failed"] != 1 ||
		called["denied"]+called["invalid"]+called["unknown"] != 0 {
		t.Fatalf("called = %v", called)
	}
}

func TestAfterToolRunsOnSettledHostResults(t *testing.T) {
	redact := replaceText("redact", func(s string) string {
		return strings.ReplaceAll(s, "sk-123", "[redacted]")
	})
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	result := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("key=sk-123"))}
	t.Run("running turn", func(t *testing.T) {
		client := &scriptedClient{
			results: []llm.Result{callResult("call-1", "lookup"), final("found")},
		}
		runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil || out.Stop.Kind != StopWaiting {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
		out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
		if err != nil || modelText(out.Tools["call-1"].Result()) != "key=[redacted]" {
			t.Fatalf("result=%+v err=%v", out.Tools["call-1"].Result(), err)
		}
		settled := settlementOf(t, out.Turn, "call-1")
		if settled.ActiveHooks == nil || len(settled.ActiveHooks.AfterTool) != 1 ||
			hasWarning(out, WarningHooksChanged) {
			t.Fatalf("settled=%+v warnings=%+v", settled, out.Warnings)
		}
		// A Settle from a handler without the hooks is recorded as a change.
		bare := runner
		bare.Hooks = Hooks{}
		out2, err := bare.Run(t.Context(), cloneTurn(out.Turn), Continue{CommandID: "go"})
		if err != nil || out2.Stop.Kind != StopCompleted {
			t.Fatalf("continue stop=%+v err=%v", out2.Stop, err)
		}
	})
	t.Run("hookless settle warns", func(t *testing.T) {
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil {
			t.Fatal(err)
		}
		bare := runner
		bare.Hooks = Hooks{}
		out, err = bare.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
		if err != nil || !hasWarning(out, WarningHooksChanged) ||
			modelText(out.Tools["call-1"].Result()) != "key=sk-123" {
			t.Fatalf("result=%+v warnings=%+v err=%v", out.Tools["call-1"].Result(), out.Warnings, err)
		}
	})
	t.Run("ended turn", func(t *testing.T) {
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err = runner.Run(t.Context(), out.Turn, Cancel{CommandID: "cancel"})
		if err != nil || out.Stop.Kind != StopCanceled {
			t.Fatalf("cancel stop=%+v err=%v", out.Stop, err)
		}
		late := Settle{CommandID: "late", CallID: "call-1", Outcome: &result}
		out, err = runner.Run(t.Context(), out.Turn, late)
		if err != nil || modelText(out.Tools["call-1"].Result()) != "key=[redacted]" {
			t.Fatalf("late result=%+v err=%v", out.Tools["call-1"].Result(), err)
		}
		if _, err := Project(out.Turn); err != nil {
			t.Fatalf("project: %v", err)
		}
	})
}

func TestReplayRejectsAResultOnRunnerWrittenOutcomes(t *testing.T) {
	deny := ToolHook{Name: "deny", Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		return Deny("no"), nil
	}}
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{secretTool(&runs)}, deny.Hooks())
	if err != nil {
		t.Fatal(err)
	}
	turn := cloneTurn(out.Turn)
	for i, step := range turn.Steps {
		if step.Kind == StepToolSettled {
			turn.Steps[i].Tool.Result = &llm.ToolResult{
				CallID: "call-1",
				Output: tool.TextOutput("forged"),
			}
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestFailOpenIsOnlyForAfterToolAndBeforeFinish(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	gate := ToolHook{Name: "gate", FailOpen: true, Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, nil
	}}
	if _, err := runHooked(t, client, nil, gate.Hooks()); err == nil {
		t.Fatal("FailOpen accepted on before_tool")
	}
	guard := FinishHook{Name: "guard", FailOpen: true, Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{}, errors.New("guard down")
	}}
	out, err := runHooked(t, client, nil, guard.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || !hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}
