package dashi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		t.Fatalf("messages = %q", seen)
	}
	sent := client.inputs[1].Messages
	if !strings.Contains(sent[len(sent)-1].Content[0].ToolResult.Output.Content[0].Text.Text, "REDACTED") {
		t.Fatalf("the model saw the raw result")
	}
	finished := finishOf(t, out.Turn, "call-1")
	if finished.Tool.Result == nil || len(finished.HookResults) != 2 ||
		finished.HookResults[0].Verdict != VerdictReplace {
		t.Fatalf("finished = %+v", finished)
	}
	if _, err := out.Turn.View(); err != nil {
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
	finished := finishOf(t, out.Turn, "call-1")
	if finished.Tool.Result != nil || len(finished.HookResults) != 0 {
		t.Fatalf("recorded a no-op replacement: %+v", finished)
	}
}

// A hook that keeps the result but gives a reason, such as a check that
// passed, leaves a no_change record; the model sees the result as it was.
func TestAfterToolKeptResultWithReasonIsRecorded(t *testing.T) {
	passed := ResultHook{Name: "scan", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
		return ResultDecision{Reason: "clean 2%", RulesVersion: "scanner-1"}, nil
	}}
	same := ResultHook{Name: "same", Func: func(_ context.Context, p ResultPlan) (ResultDecision, error) {
		r := p.Result
		return ResultDecision{Replace: &r, Reason: "looked"}, nil
	}}
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{secretTool(&runs)},
		Hooks{AfterTool: []ResultHook{passed, same}})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	finished := finishOf(t, out.Turn, "call-1")
	want := []HookResult{
		{Point: HookAfterTool, Name: "scan", Verdict: VerdictNoChange, RulesVersion: "scanner-1", Reason: "clean 2%"},
		{Point: HookAfterTool, Name: "same", Verdict: VerdictNoChange, Reason: "looked"},
	}
	if finished.Tool.Result != nil || len(finished.HookResults) != 2 ||
		finished.HookResults[0] != want[0] || finished.HookResults[1] != want[1] {
		t.Fatalf("finished = %+v", finished)
	}
	if got := modelText(out.Tools["call-1"].Result()); got != "key=sk-123 ok" {
		t.Fatalf("model result = %q", got)
	}
	if _, err := out.Turn.View(); err != nil {
		t.Fatal(err)
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
			want := `The tool ran, but its result was withheld: check "check" failed.`
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
		Client: client,
		Tools:  tool.List{secretTool(&runs)},
		Hooks:  slow.Hooks(),
	}).Run(ctx, Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopInterrupted || runs != 1 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
	}
	view := out.Tools["call-1"]
	if view.Status != ToolFinished || view.Outcome.State != tool.StateSucceeded ||
		!strings.Contains(modelText(view.Result()), "withheld") ||
		!hasWarning(out, WarningHookFailed) {
		t.Fatalf("view=%+v result=%+v warnings=%+v", view, view.Result(), out.Warnings)
	}
	if _, err := TurnMessagesFull(out.Turn); err != nil {
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

func TestAfterToolRunsOnFinishedHostResults(t *testing.T) {
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
		runner := Runner{Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil || out.Stop.Kind != StopWaiting {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
		out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
		if err != nil || modelText(out.Tools["call-1"].Result()) != "key=[redacted]" {
			t.Fatalf("result=%+v err=%v", out.Tools["call-1"].Result(), err)
		}
		finished := finishOf(t, out.Turn, "call-1")
		if finished.ActiveHooks == nil || len(finished.ActiveHooks.AfterTool) != 1 ||
			hasWarning(out, WarningHooksChanged) {
			t.Fatalf("finished=%+v warnings=%+v", finished, out.Warnings)
		}
		// A Resolve from a handler without the hooks is recorded as a change.
		bare := runner
		bare.Hooks = Hooks{}
		out2, err := bare.Run(t.Context(), cloneTurn(out.Turn), Continue{CommandID: "go"})
		if err != nil || out2.Stop.Kind != StopCompleted {
			t.Fatalf("continue stop=%+v err=%v", out2.Stop, err)
		}
	})
	t.Run("hookless finish warns", func(t *testing.T) {
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		runner := Runner{Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     testAgent(),
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
		runner := Runner{Client: client, Tools: tool.List{external}, Hooks: redact.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err = runner.Run(t.Context(), out.Turn, Cancel{CommandID: "cancel"})
		if err != nil || out.Stop.Kind != StopCanceled {
			t.Fatalf("cancel stop=%+v err=%v", out.Stop, err)
		}
		late := Resolve{CommandID: "late", CallID: "call-1", Outcome: &result}
		out, err = runner.Run(t.Context(), out.Turn, late)
		if err != nil || modelText(out.Tools["call-1"].Result()) != "key=[redacted]" {
			t.Fatalf("late result=%+v err=%v", out.Tools["call-1"].Result(), err)
		}
		if _, err := TurnMessagesFull(out.Turn); err != nil {
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
		if step.Kind == StepToolFinished {
			turn.Steps[i].Tool.Result = &llm.ToolResult{
				CallID: "call-1",
				Output: tool.TextOutput("forged"),
			}
		}
	}
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestFailOpenIsNotForModelOrSend(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	model := ModelHook{Name: "model", FailOpen: true, Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{}, nil
	}}
	if _, err := runHooked(t, client, nil, model.Hooks()); err == nil {
		t.Fatal("FailOpen accepted on before_model")
	}
	send := SendHook{Name: "send", FailOpen: true, Func: func(context.Context, SendPlan) (SendDecision, error) {
		return SendDecision{}, nil
	}}
	if _, err := runHooked(t, client, nil, send.Hooks()); err == nil {
		t.Fatal("FailOpen accepted on before_send")
	}
	guard := FinishHook{Name: "guard", FailOpen: true, Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{}, errors.New("guard down")
	}}
	out, err := runHooked(t, client, nil, guard.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || !hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}

// A failing BeforeTool hook with FailOpen counts as no opinion: the call
// runs, later hooks still decide, the model sees nothing of the failure,
// and a warning is recorded. Without FailOpen the call is denied.
func TestBeforeToolFailOpenRunsTheCall(t *testing.T) {
	advice := ToolHook{Name: "advice", FailOpen: true, Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, errors.New("advisor down")
	}}
	invalid := ToolHook{Name: "invalid", FailOpen: true, Func: func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{Kind: "maybe"}, nil
	}}
	t.Run("runs", func(t *testing.T) {
		runs := 0
		client := &scriptedClient{
			results: []llm.Result{callResult("call-1", "remote"), final("done")},
		}
		out, err := runHooked(t, client, tool.List{secretTool(&runs)},
			Hooks{BeforeTool: []ToolHook{advice, invalid}})
		if err != nil || out.Stop.Kind != StopCompleted || runs != 1 {
			t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
		}
		failed := 0
		for _, w := range out.Warnings {
			if w.Code == WarningHookFailed {
				failed++
			}
		}
		if failed != 2 {
			t.Fatalf("warnings = %+v", out.Warnings)
		}
		if got := toolResultText(t, out.Turn, "call-1"); strings.Contains(got, "advice") {
			t.Fatalf("model saw the failure: %q", got)
		}
		if _, err := out.Turn.View(); err != nil {
			t.Fatalf("replay: %v", err)
		}
	})
	t.Run("later hooks still decide", func(t *testing.T) {
		runs := 0
		client := &scriptedClient{
			results: []llm.Result{callResult("call-1", "remote"), final("done")},
		}
		hold := ToolHook{Name: "hold", Func: func(context.Context, ToolPlan) (ToolDecision, error) {
			return NeedsApproval("ask first"), nil
		}}
		out, err := runHooked(t, client, tool.List{secretTool(&runs)},
			Hooks{BeforeTool: []ToolHook{advice, hold}})
		if err != nil || runs != 0 || len(out.Pending) != 1 {
			t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
		}
	})
	t.Run("without FailOpen denies", func(t *testing.T) {
		runs := 0
		client := &scriptedClient{
			results: []llm.Result{callResult("call-1", "remote"), final("done")},
		}
		strict := advice
		strict.FailOpen = false
		out, err := runHooked(t, client, tool.List{secretTool(&runs)}, strict.Hooks())
		if err != nil || runs != 0 || !hasWarning(out, WarningHookFailed) {
			t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, runs, err)
		}
	})
}

func TestAfterToolCannotChangeTheRecordedOutcome(t *testing.T) {
	costly := tool.Tool{
		Definition: tool.Definition{
			Name:   "remote",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			out := tool.Succeeded("ok")
			out.Cost = &tool.Cost{Amount: amount("5"), Currency: "USD"}
			return out, nil
		}),
	}
	meddle := ResultHook{Name: "meddle", Func: func(_ context.Context, p ResultPlan) (ResultDecision, error) {
		p.Outcome.Cost.Amount = 0
		p.Outcome.Output.Content[0].Text.Text = "changed"
		return ResultDecision{}, nil
	}}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{costly}, meddle.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	outcome := out.Tools["call-1"].Outcome
	if outcome.Cost.Amount != amount("5") || modelText(out.Tools["call-1"].Result()) != "ok" {
		t.Fatalf("outcome = %+v cost=%+v", outcome, outcome.Cost)
	}

	wait := tool.Outcome{State: tool.StateWaiting, Wait: &tool.Wait{Reason: "r"}, Handle: &tool.Handle{ID: "h"}}
	copied := wait.Clone()
	copied.Wait.Reason = "x"
	copied.Handle.ID = "y"
	if wait.Wait.Reason != "r" || wait.Handle.ID != "h" {
		t.Fatal("Clone shares Wait or Handle")
	}
}

func TestAfterToolFailOpenKeepsTheResultWhenTheRunEnds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closed   bool
		withheld bool
	}{
		{name: "only fail-open hooks left", withheld: false},
		{name: "a fail-closed hook left", closed: true, withheld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			redact := replaceText("redact", func(s string) string {
				return strings.ReplaceAll(s, "sk-123", "[redacted]")
			})
			enrich := ResultHook{Name: "enrich", FailOpen: true, Func: func(
				ctx context.Context,
				_ ResultPlan,
			) (ResultDecision, error) {
				cancel()
				<-ctx.Done()
				return ResultDecision{}, ctx.Err()
			}}
			hooks := []ResultHook{redact, enrich}
			if tc.closed {
				hooks = append(hooks, replaceText("strict", func(s string) string { return s }))
			}
			runs := 0
			client := &scriptedClient{
				results: []llm.Result{callResult("call-1", "remote"), final("done")},
			}
			out, err := (Runner{
				Client: client,
				Tools:  tool.List{secretTool(&runs)},
				Hooks:  Hooks{AfterTool: hooks},
			}).Run(ctx, Turn{ID: "t1"}, Start{
				CommandID: "start",
				Agent:     testAgent(),
				Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
			})
			if err != nil || out.Stop.Kind != StopInterrupted ||
				!hasWarning(out, WarningHookFailed) {
				t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
			}
			got := modelText(out.Tools["call-1"].Result())
			if tc.withheld != strings.Contains(got, "withheld") ||
				!tc.withheld && got != "key=[redacted] ok" {
				t.Fatalf("model result = %q", got)
			}
		})
	}
}

func TestAfterToolOnResolveIgnoresTheTurnDeadline(t *testing.T) {
	strictRedact := ResultHook{Name: "redact", Func: func(
		ctx context.Context,
		p ResultPlan,
	) (ResultDecision, error) {
		if err := ctx.Err(); err != nil {
			return ResultDecision{}, err
		}
		r := p.Result
		r.Output = r.Output.MapText(func(s string) string {
			return strings.ReplaceAll(s, "sk-123", "[redacted]")
		})
		return ResultDecision{Replace: &r}, nil
	}}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	agent := testAgent()
	agent.Limits.Deadline = time.Now().Add(300 * time.Millisecond)
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	runner := Runner{Client: client, Tools: tool.List{external}, Hooks: strictRedact.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	time.Sleep(time.Until(agent.Limits.Deadline) + 50*time.Millisecond)
	result := tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("key=sk-123"))}
	out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
	if err != nil || modelText(out.Tools["call-1"].Result()) != "key=[redacted]" {
		t.Fatalf("result=%+v err=%v", out.Tools["call-1"].Result(), err)
	}
}

func TestAfterToolRunsOnFatalAndResultUnknownResults(t *testing.T) {
	calls := 0
	watch := ResultHook{Name: "watch", Func: func(context.Context, ResultPlan) (ResultDecision, error) {
		calls++
		return ResultDecision{}, nil
	}}
	t.Run("fatal", func(t *testing.T) {
		calls = 0
		fatal := tool.Tool{
			Definition: tool.Definition{
				Name:   "remote",
				Schema: []byte(`{"type":"object"}`),
				Kind:   tool.KindLocal,
			},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				return tool.Outcome{}, tool.Fatal(errors.New("disk full"))
			}),
		}
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote")}}
		out, err := runHooked(t, client, tool.List{fatal}, watch.Hooks())
		if err != nil || out.Stop.Reason != StopReasonToolFatal || calls != 1 {
			t.Fatalf("stop=%+v calls=%d err=%v", out.Stop, calls, err)
		}
	})
	t.Run("result unknown", func(t *testing.T) {
		calls = 0
		panicky := tool.Tool{
			Definition: tool.Definition{
				Name:   "remote",
				Schema: []byte(`{"type":"object"}`),
				Kind:   tool.KindLocal,
			},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				panic("boom")
			}),
		}
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote")}}
		runner := Runner{Client: client, Tools: tool.List{panicky}, Hooks: watch.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil || out.Stop.Kind != StopResultUnknown || calls != 0 {
			t.Fatalf("stop=%+v calls=%d err=%v", out.Stop, calls, err)
		}
		result := tool.Succeeded("resolved")
		out, err = runner.Run(t.Context(), out.Turn, out.Pending[0].Result(result))
		if err != nil || calls != 1 || out.Tools["call-1"].Status != ToolFinished {
			t.Fatalf("status=%s calls=%d err=%v", out.Tools["call-1"].Status, calls, err)
		}
	})
}

func TestReplayRejectsAResultOffAFinish(t *testing.T) {
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{secretTool(&runs)}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	turn := cloneTurn(out.Turn)
	for i, step := range turn.Steps {
		if step.Kind == StepToolStarted {
			turn.Steps[i].Tool.Result = &llm.ToolResult{
				CallID: "call-1",
				Output: tool.TextOutput("forged"),
			}
		}
	}
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestAfterToolRecordsReasonAndRulesVersion(t *testing.T) {
	screen := ResultHook{Name: "screen", Func: func(
		_ context.Context,
		p ResultPlan,
	) (ResultDecision, error) {
		r := p.Result
		r.Output = tool.TextOutput("withheld: possible injection")
		return ResultDecision{
			Replace:      &r,
			Reason:       "P(injection)=0.88",
			RulesVersion: "jev-1.13.0",
		}, nil
	}}
	runs := 0
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{secretTool(&runs)}, screen.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	got := finishOf(t, out.Turn, "call-1").HookResults
	if len(got) != 1 || got[0].Verdict != VerdictReplace ||
		got[0].Reason != "P(injection)=0.88" || got[0].RulesVersion != "jev-1.13.0" {
		t.Fatalf("hook results = %+v", got)
	}
	if strings.Contains(modelText(out.Tools["call-1"].Result()), "0.88") {
		t.Fatal("the model saw the reason")
	}
}
