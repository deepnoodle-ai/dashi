package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// valueTool is a local tool whose arguments are {"value": integer}; runs
// counts executions and args records what each execution received.
type valueTool struct {
	runs int
	args []string
}

func (v *valueTool) binding() tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name: "remote",
			Schema: []byte(`{"type":"object","properties":{"value":{"type":"integer"}},` +
				`"required":["value"]}`),
			Kind: tool.KindLocal,
		},
		Executor: execFunc(func(_ context.Context, c tool.Call) (tool.Outcome, error) {
			v.runs++
			v.args = append(v.args, string(c.Args))
			return tool.Succeeded("ran"), nil
		}),
	}
}

func runHooked(
	t *testing.T,
	client *scriptedClient,
	tools tool.Toolset,
	hooks Hooks,
	opts ...RunOption,
) (RunResult, error) {
	t.Helper()
	return (Runner{
		Client: client,
		Tools:  tools,
		Hooks:  hooks,
	}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
		opts...,
	)
}

func toolHook(
	name string,
	f func(context.Context, ToolPlan) (ToolDecision, error),
) ToolHook {
	return ToolHook{Name: name, Func: f}
}

func allHookResults(turn Turn) []HookResult {
	var out []HookResult
	for _, step := range turn.Steps {
		out = append(out, step.HookResults...)
	}
	return out
}

func hasWarning(out RunResult, code WarningCode) bool {
	return slices.ContainsFunc(out.Warnings, func(w Warning) bool {
		return w.Code == code
	})
}

// toolResultText returns what the model saw as the result of call id.
func toolResultText(t *testing.T, turn Turn, id string) string {
	t.Helper()
	messages, err := TurnMessagesFull(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		for _, part := range m.Content {
			if part.Kind == llm.ContentToolResult && part.ToolResult.CallID == id {
				var text strings.Builder
				for _, block := range part.ToolResult.Output.Content {
					if block.Text != nil {
						text.WriteString(block.Text.Text)
					}
				}
				return text.String()
			}
		}
	}
	t.Fatalf("no result for %s", id)
	return ""
}

func TestNoHooksRunsBoundToolWithoutHookRecords(t *testing.T) {
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, Hooks{})
	if err != nil || out.Stop.Kind != StopCompleted || remote.runs != 1 {
		t.Fatalf("out=%+v err=%v runs=%d", out.Stop, err, remote.runs)
	}
	verdict := out.Tools["call-1"].Verdict
	if verdict != (ToolVerdict{Kind: DecisionAllow, Reason: "no hook opinion"}) {
		t.Fatalf("verdict = %+v", verdict)
	}
	for _, step := range out.Turn.Steps {
		if len(step.HookResults) > 0 || step.ActiveHooks != nil {
			t.Fatalf("step %s records hooks: %+v", step.Kind, step)
		}
	}
}

func TestBeforeToolCombining(t *testing.T) {
	later := 0
	cases := []struct {
		name    string
		hooks   []ToolHook
		kind    DecisionKind
		hook    string
		runs    int
		laterOK bool
	}{
		{
			name: "allow names the first allowing hook",
			hooks: []ToolHook{
				toolHook("quiet", func(context.Context, ToolPlan) (ToolDecision, error) {
					return ToolDecision{}, nil
				}),
				toolHook("ask", func(context.Context, ToolPlan) (ToolDecision, error) {
					return Allow("operator approved"), nil
				}),
			},
			kind: DecisionAllow,
			hook: "ask",
			runs: 1,
		},
		{
			name: "defer beats an earlier allow",
			hooks: []ToolHook{
				toolHook("ask", func(context.Context, ToolPlan) (ToolDecision, error) {
					return Allow("fine"), nil
				}),
				toolHook("gate", func(context.Context, ToolPlan) (ToolDecision, error) {
					return NeedsApproval("needs a person"), nil
				}),
			},
			kind: DecisionNeedsApproval,
			hook: "gate",
		},
		{
			name: "the first deny ends the chain",
			hooks: []ToolHook{
				toolHook("deny", func(context.Context, ToolPlan) (ToolDecision, error) {
					return Deny("no"), nil
				}),
				toolHook("later", func(context.Context, ToolPlan) (ToolDecision, error) {
					later++
					return ToolDecision{}, nil
				}),
			},
			kind:    DecisionDeny,
			hook:    "deny",
			laterOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			later = 0
			remote := &valueTool{}
			client := &scriptedClient{
				results: []llm.Result{callResult("call-1", "remote"), final("done")},
			}
			out, err := runHooked(
				t,
				client,
				tool.List{remote.binding()},
				Hooks{BeforeTool: tc.hooks},
			)
			if err != nil {
				t.Fatal(err)
			}
			v := out.Tools["call-1"].Verdict
			if v.Kind != tc.kind || v.Hook != tc.hook || remote.runs != tc.runs {
				t.Fatalf("verdict=%+v runs=%d", v, remote.runs)
			}
			if tc.laterOK && later != 0 {
				t.Fatalf("a hook ran after a denial")
			}
		})
	}
}

func TestBeforeToolFailureDeniesWithWarning(t *testing.T) {
	for name, f := range map[string]func(context.Context, ToolPlan) (ToolDecision, error){
		"error": func(context.Context, ToolPlan) (ToolDecision, error) {
			return ToolDecision{}, errors.New("secret backend detail")
		},
		"panic": func(context.Context, ToolPlan) (ToolDecision, error) {
			panic("secret backend detail")
		},
		"invalid kind": func(context.Context, ToolPlan) (ToolDecision, error) {
			return ToolDecision{Kind: "maybe"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			remote := &valueTool{}
			client := &scriptedClient{
				results: []llm.Result{callResult("call-1", "remote"), final("done")},
			}
			out, err := runHooked(
				t,
				client,
				tool.List{remote.binding()},
				toolHook("check", f).Hooks(),
			)
			if err != nil || out.Stop.Kind != StopCompleted || remote.runs != 0 {
				t.Fatalf("stop=%+v err=%v runs=%d", out.Stop, err, remote.runs)
			}
			seen := toolResultText(t, out.Turn, "call-1")
			if seen != "Denied: check check failed." {
				t.Fatalf("model saw %q", seen)
			}
			if !hasWarning(out, WarningHookFailed) {
				t.Fatalf("warnings = %+v", out.Warnings)
			}
			results := allHookResults(out.Turn)
			if len(results) != 1 || results[0].Verdict != "failed" ||
				results[0].Error == "" {
				t.Fatalf("results = %+v", results)
			}
		})
	}
}

func TestOnToolDecodesAndDeniesOnMismatch(t *testing.T) {
	type good struct{ Value int }
	type bad struct{ Value string }
	var seen int
	match := OnTool("remote", func(_ context.Context, _ ToolPlan, in good) (ToolDecision, error) {
		seen = in.Value
		return Allow("checked"), nil
	})
	other := OnTool("other", func(context.Context, ToolPlan, good) (ToolDecision, error) {
		t.Fatal("hook ran for another tool")
		return ToolDecision{}, nil
	})
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(
		t,
		client,
		tool.List{remote.binding()},
		JoinHooks(match.Hooks(), other.Hooks()),
	)
	if err != nil || seen != 1 || remote.runs != 1 ||
		out.Tools["call-1"].Verdict.Hook != "remote" {
		t.Fatalf("seen=%d runs=%d verdict=%+v err=%v", seen, remote.runs, out.Tools["call-1"].Verdict, err)
	}

	mismatch := OnTool("remote", func(context.Context, ToolPlan, bad) (ToolDecision, error) {
		return Allow("never"), nil
	})
	remote = &valueTool{}
	client = &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err = runHooked(t, client, tool.List{remote.binding()}, mismatch.Hooks())
	if err != nil || remote.runs != 0 || out.Tools["call-1"].Verdict.Kind != DecisionDeny {
		t.Fatalf("mismatch verdict=%+v runs=%d err=%v", out.Tools["call-1"].Verdict, remote.runs, err)
	}
}

func TestConvergingRewriteRunsOnce(t *testing.T) {
	calls := 0
	normalize := toolHook("normalize", func(context.Context, ToolPlan) (ToolDecision, error) {
		calls++
		return Rewrite(map[string]int{"value": 2})
	})
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, normalize.Hooks())
	if err != nil || remote.runs != 1 || remote.args[0] != `{"value":2}` {
		t.Fatalf("runs=%d args=%v err=%v", remote.runs, remote.args, err)
	}
	call := out.Tools["call-1"].Call
	if string(call.RequestedArgs) != `{"value":1}` || calls != 2 {
		t.Fatalf("requested=%s calls=%d", call.RequestedArgs, calls)
	}
	results := allHookResults(out.Turn)
	if len(results) != 1 || results[0].Verdict != "rewrite" || results[0].Pass != 0 {
		t.Fatalf("results = %+v", results)
	}
}

func TestRunawayRewriteIsDeniedAndTurnContinues(t *testing.T) {
	n := 1
	runaway := toolHook("runaway", func(context.Context, ToolPlan) (ToolDecision, error) {
		n++
		return Rewrite(map[string]int{"value": n})
	})
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, runaway.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || remote.runs != 0 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, remote.runs, err)
	}
	v := out.Tools["call-1"].Verdict
	if v.Kind != DecisionDeny ||
		v.Reason != "Not run: a check kept changing the arguments." ||
		!hasWarning(out, WarningHookFailed) {
		t.Fatalf("verdict=%+v warnings=%+v", v, out.Warnings)
	}
	results := allHookResults(out.Turn)
	if len(results) != maxRewrites+1 || results[maxRewrites].Pass != maxRewrites {
		t.Fatalf("results = %+v", results)
	}
}

func TestRewriteOutsideSchemaIsDenied(t *testing.T) {
	bad := toolHook("bad", func(context.Context, ToolPlan) (ToolDecision, error) {
		return Rewrite(map[string]string{"value": "two"})
	})
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, bad.Hooks())
	if err != nil || remote.runs != 0 ||
		out.Tools["call-1"].Verdict.Kind != DecisionDeny {
		t.Fatalf("verdict=%+v runs=%d err=%v", out.Tools["call-1"].Verdict, remote.runs, err)
	}
}

func TestHookErrorAfterContextEndsIsAContextStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stall := toolHook("prompt", func(ctx context.Context, _ ToolPlan) (ToolDecision, error) {
		cancel()
		<-ctx.Done()
		return ToolDecision{}, ctx.Err()
	})
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  stall.Hooks(),
	}).Run(ctx, Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopInterrupted || remote.runs != 0 {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if results := allHookResults(out.Turn); len(results) != 0 {
		t.Fatalf("recorded %+v for an ended context", results)
	}
	if out.Tools["call-1"].Status != ToolRequested {
		t.Fatalf("call status = %s", out.Tools["call-1"].Status)
	}
}

func TestBeforeModelChainsSettingsAndSkipsFailures(t *testing.T) {
	var seen []int
	hooks := Hooks{BeforeModel: []ModelHook{
		{Name: "broken", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			return ModelDecision{}, errors.New("flaky")
		}},
		{Name: "first", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			s := p.Input.Settings
			s.MaxTokens = 50
			return ModelDecision{Settings: &s}, nil
		}},
		{Name: "second", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			seen = append(seen, p.Input.Settings.MaxTokens)
			return ModelDecision{}, nil
		}},
		{Name: "bad-stop", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			return ModelDecision{Stop: &Stop{
				Kind:  StopLimited,
				Limit: &LimitDetail{Kind: LimitDeadline, Limit: 1, Used: 2},
			}}, nil
		}},
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, nil, hooks)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if !slices.Equal(seen, []int{50}) || client.inputs[0].Settings.MaxTokens != 50 {
		t.Fatalf("seen=%v sent=%d", seen, client.inputs[0].Settings.MaxTokens)
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
	callStart := out.Turn.Steps[1]
	if callStart.Kind != StepModelCallStarted || len(callStart.HookResults) != 3 {
		t.Fatalf("call start = %+v", callStart)
	}
}

func TestBeforeModelStopDiscardsAddedMessages(t *testing.T) {
	item := ContextItem{
		Origin:  ContextRuntime,
		Name:    "note",
		Message: llm.UserMessage(llm.Text("note")),
	}
	hooks := Hooks{BeforeModel: []ModelHook{
		{Name: "inject", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			return ModelDecision{AddMessages: []ContextItem{item}}, nil
		}},
		{Name: "stop", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			return ModelDecision{Stop: &Stop{Kind: StopRefused, Reason: "closed"}}, nil
		}},
	}}
	client := &scriptedClient{}
	out, err := runHooked(t, client, nil, hooks)
	if err != nil || out.Stop.Kind != StopRefused || client.sends != 0 {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepMessagesAdded {
			t.Fatal("a stopped chain delivered context")
		}
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	if len(last.HookResults) != 2 || last.HookResults[1].Verdict != "stop" {
		t.Fatalf("stop step results = %+v", last.HookResults)
	}
}

func TestContextViewTracksLiveNamedItems(t *testing.T) {
	var sawCurrent []bool
	inject := ModelHook{Name: "inject", Func: func(
		_ context.Context,
		p ModelPlan,
	) (ModelDecision, error) {
		_, current := p.Turn.Context(ContextRuntime, "clock")
		sawCurrent = append(sawCurrent, current)
		if current {
			return ModelDecision{}, nil
		}
		return ModelDecision{AddMessages: []ContextItem{{
			Origin:   ContextRuntime,
			Name:     "clock",
			Snapshot: true,
			Message:  llm.UserMessage(llm.Text("noon")),
		}}}, nil
	}}
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, inject.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if !slices.Equal(sawCurrent, []bool{false, true}) {
		t.Fatalf("saw current = %v", sawCurrent)
	}
	delivered := 0
	for _, step := range out.Turn.Steps {
		if step.Kind == StepMessagesAdded {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered %d times", delivered)
	}
}

func TestBeforeSendReservationDenyAndFailure(t *testing.T) {
	admit := func(name, terms string) SendHook {
		return SendHook{Name: name, Func: func(context.Context, SendPlan) (SendDecision, error) {
			return SendDecision{Reservation: json.RawMessage(terms)}, nil
		}}
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, nil, Hooks{BeforeSend: []SendHook{
		admit("credits", `{"hold":1}`),
		admit("quota", `{"slot":"a"}`),
	}})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	admission := out.Turn.Steps[1].Model.Reservation
	if len(admission) != 2 || string(admission["quota"]) != `{"slot":"a"}` {
		t.Fatalf("reservation = %v", admission)
	}

	deny := SendHook{Name: "funds", Func: func(context.Context, SendPlan) (SendDecision, error) {
		return SendDecision{
			Reason: "out of credits",
			Limit:  &LimitDetail{Kind: LimitHost, Code: "credits"},
		}, nil
	}}
	client = &scriptedClient{}
	out, err = runHooked(t, client, nil, deny.Hooks())
	if err != nil || out.Stop.Kind != StopLimited || out.Stop.Limit.Kind != LimitHost ||
		client.sends != 0 {
		t.Fatalf("deny stop=%+v err=%v", out.Stop, err)
	}

	terminal := SendHook{Name: "clock", Func: func(context.Context, SendPlan) (SendDecision, error) {
		return SendDecision{Limit: &LimitDetail{Kind: LimitModelCalls, Limit: 1, Used: 1}}, nil
	}}
	client = &scriptedClient{}
	out, err = runHooked(t, client, nil, terminal.Hooks())
	if err != nil || out.Stop.Kind != StopFailed || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("terminal limit stop=%+v err=%v", out.Stop, err)
	}
}

func TestHookFailedStopResumesWithoutCountingResume(t *testing.T) {
	fail := true
	flaky := SendHook{Name: "flaky", Func: func(context.Context, SendPlan) (SendDecision, error) {
		if fail {
			return SendDecision{}, errors.New("ledger down")
		}
		return SendDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client, Hooks: flaky.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopFailed ||
		out.Stop.Reason != StopReasonHookFailed || client.sends != 0 {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	if len(last.HookResults) != 1 || last.HookResults[0].Error != "ledger down" {
		t.Fatalf("results = %+v", last.HookResults)
	}
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopCompleted || out.Resumes != 0 {
		t.Fatalf("resumed stop=%+v interruptions=%d err=%v", out.Stop, out.Resumes, err)
	}
}

func TestBeforeFinishAddsMessagesAcceptsEmptyAndRetries(t *testing.T) {
	t.Run("deliver continues", func(t *testing.T) {
		asked := 0
		green := FinishHook{Name: "tests", Func: func(
			_ context.Context,
			p FinishPlan,
		) (FinishDecision, error) {
			asked++
			if p.Reply == "draft" {
				return FinishDecision{AddMessages: []ContextItem{{
					Origin:  ContextRuntime,
					Message: llm.UserMessage(llm.Text("tests fail")),
				}}}, nil
			}
			return FinishDecision{}, nil
		}}
		client := &scriptedClient{results: []llm.Result{final("draft"), final("fixed")}}
		out, err := runHooked(t, client, nil, green.Hooks())
		if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "fixed" ||
			asked != 2 {
			t.Fatalf("stop=%+v answer=%q asked=%d err=%v", out.Stop, out.Reply, asked, err)
		}
	})
	t.Run("accept empty", func(t *testing.T) {
		ok := FinishHook{Name: "ok", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			return FinishDecision{AcceptEmpty: true}, nil
		}}
		client := &scriptedClient{results: []llm.Result{final("")}}
		out, err := runHooked(t, client, nil, ok.Hooks())
		if err != nil || out.Stop.Kind != StopCompleted {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
	})
	t.Run("failure retries on the same answer", func(t *testing.T) {
		fail := true
		asked := 0
		guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			asked++
			if fail {
				panic("guard crashed")
			}
			return FinishDecision{}, nil
		}}
		client := &scriptedClient{results: []llm.Result{final("answer")}}
		runner := Runner{Client: client, Hooks: guard.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil || out.Stop.Reason != StopReasonHookFailed {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
		fail = false
		out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
		if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 ||
			asked != 2 || out.Reply != "answer" {
			t.Fatalf("resumed=%+v sends=%d asked=%d err=%v", out.Stop, client.sends, asked, err)
		}
	})
}

func TestBeforeFinishSeesStructuredOutputOrItsError(t *testing.T) {
	var plans []FinishPlan
	fixer := FinishHook{Name: "fixer", Func: func(
		_ context.Context,
		p FinishPlan,
	) (FinishDecision, error) {
		plans = append(plans, p)
		if p.OutputErr != nil {
			return FinishDecision{AddMessages: []ContextItem{{
				Origin:  ContextRuntime,
				Message: llm.UserMessage(llm.Text("return valid JSON")),
			}}}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		final("not json"),
		final(`{"name":"Ada"}`),
	}}
	agent := testAgent()
	agent.OutputSchema = json.RawMessage(
		`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
	)
	out, err := (Runner{Client: client, Hooks: fixer.Hooks()}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("who?"))},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(plans) != 2 || plans[0].OutputErr == nil ||
		string(plans[1].Output) != `{"name":"Ada"}` {
		t.Fatalf("plans = %+v", plans)
	}
}

func TestBeforeFinishMessagesDroppedOnReservedFinalReply(t *testing.T) {
	always := FinishHook{Name: "more", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{AddMessages: []ContextItem{{
			Origin:  ContextRuntime,
			Message: llm.UserMessage(llm.Text("keep going")),
		}}}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 2
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("last"),
	}}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  always.Hooks(),
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Kind != StopLimited ||
		!hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}

func TestBeforeFinishMessagesDroppedWithoutReserveAtLastModelCall(t *testing.T) {
	more := FinishHook{Name: "more", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{AddMessages: []ContextItem{{
			Message: llm.UserMessage(llm.Text("keep going")),
		}}}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 1
	agent.Limits.NoReservedFinalReply = true
	client := &scriptedClient{results: []llm.Result{final("only")}}
	out, err := (Runner{Client: client, Hooks: more.Hooks()}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "only" ||
		!hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepLoopContinued {
			t.Fatal("recorded a continuation no model call could use")
		}
	}
}

func TestBeforeModelInvalidSettingsAreSkipped(t *testing.T) {
	hooks := Hooks{BeforeModel: []ModelHook{
		{Name: "first", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			s := p.Input.Settings
			s.MaxTokens = 50
			return ModelDecision{Settings: &s}, nil
		}},
		{Name: "broken", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
			s := p.Input.Settings
			s.MaxTokens = -1
			return ModelDecision{Settings: &s}, nil
		}},
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, nil, hooks)
	if err != nil || out.Stop.Kind != StopCompleted ||
		client.inputs[0].Settings.MaxTokens != 50 ||
		!hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v sent=%d warnings=%+v err=%v", out.Stop, client.inputs[0].Settings.MaxTokens, out.Warnings, err)
	}
}

func TestBeforeFinishMessagesCannotSpendTheReservedCall(t *testing.T) {
	more := FinishHook{Name: "more", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{AddMessages: []ContextItem{{
			Message: llm.UserMessage(llm.Text("keep going")),
		}}}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 2
	client := &scriptedClient{results: []llm.Result{final("first"), final("unused")}}
	out, err := (Runner{Client: client, Hooks: more.Hooks()}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "first" ||
		client.sends != 1 || !hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v sends=%d warnings=%+v err=%v", out.Stop, client.sends, out.Warnings, err)
	}
}

func TestRoomForNormalCallCountsACallInFlight(t *testing.T) {
	s := &replayState{}
	s.modelCallCap = 8
	for _, tc := range []struct {
		finished int
		inFlight bool
		reserve  bool
		want     bool
	}{
		{finished: 6, reserve: true, want: true},
		{finished: 7, reserve: true, want: false},
		{finished: 6, inFlight: true, reserve: true, want: false},
		{finished: 7, reserve: false, want: true},
		{finished: 7, inFlight: true, reserve: false, want: false},
	} {
		s.ModelCalls = tc.finished
		s.modelCallStart = ""
		if tc.inFlight {
			s.modelCallStart = "t1:model:x"
		}
		s.Agent.Limits.NoReservedFinalReply = !tc.reserve
		if got := s.roomForNormalCall(); got != tc.want {
			t.Fatalf("%+v: room = %v", tc, got)
		}
	}
}

func TestFinishAddedMessagesCheckTheCostCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   string
		sends int
	}{
		{name: "room for another call", cap: "1", sends: 2},
		{name: "cap too tight", cap: "0.0013", sends: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			green := FinishHook{Name: "tests", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
				asked++
				if asked == 1 {
					return FinishDecision{AddMessages: []ContextItem{{
						Message: llm.UserMessage(llm.Text("tests fail")),
					}}}, nil
				}
				return FinishDecision{}, nil
			}}
			client := newPricedClient(final("draft"), final("fixed"))
			out, err := runBudgetTurnErr(
				client,
				&Budget{MaxCost: &Money{Amount: amount(tc.cap), Currency: "USD"}},
				llm.Settings{MaxTokens: 100},
				nil,
				green.Hooks(),
			)
			if err != nil || out.Stop.Kind != StopCompleted || client.sends != tc.sends {
				t.Fatalf("stop=%+v sends=%d spend=%+v err=%v", out.Stop, client.sends, out.Spend, err)
			}
			if dropped := hasWarning(out, WarningMessagesDropped); dropped != (tc.sends == 1) {
				t.Fatalf("warnings = %+v", out.Warnings)
			}
		})
	}
}

func TestHooksValidation(t *testing.T) {
	noop := func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, nil
	}
	cases := map[string]Hooks{
		"empty name": toolHook("", noop).Hooks(),
		"nil func":   ToolHook{Name: "x"}.Hooks(),
		"duplicate":  JoinHooks(toolHook("x", noop).Hooks(), toolHook("x", noop).Hooks()),
	}
	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			client := &scriptedClient{results: []llm.Result{final("done")}}
			out, err := runHooked(t, client, nil, hooks)
			if err == nil || len(out.Turn.Steps) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
	t.Run("duplicate added by WithHooks", func(t *testing.T) {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		_, err := runHooked(
			t,
			client,
			nil,
			toolHook("x", noop).Hooks(),
			WithHooks(toolHook("x", noop).Hooks()),
		)
		if err == nil {
			t.Fatal("a duplicate across Runner and WithHooks was accepted")
		}
	})
}

func TestJoinHooksNeverAliases(t *testing.T) {
	noop := func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, nil
	}
	base := Hooks{BeforeTool: make([]ToolHook, 1, 4)}
	base.BeforeTool[0] = toolHook("a", noop)
	one := JoinHooks(base, toolHook("b", noop).Hooks())
	two := JoinHooks(base, toolHook("c", noop).Hooks())
	if one.BeforeTool[1].Name != "b" || two.BeforeTool[1].Name != "c" ||
		len(base.BeforeTool) != 1 {
		t.Fatalf("one=%v two=%v", hookNames(one.BeforeTool), hookNames(two.BeforeTool))
	}
}

func TestJoinHooksSkipsNil(t *testing.T) {
	noop := func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, nil
	}
	var extra HookSet
	var typed *ModelHook
	got := JoinHooks(toolHook("a", noop), extra, nil, typed)
	if names := hookNames(got.BeforeTool); !slices.Equal(names, []string{"a"}) {
		t.Fatalf("got %v; want [a]", names)
	}
}

func TestActiveHooksRecordedAndChangeWarned(t *testing.T) {
	noop := func(context.Context, ToolPlan) (ToolDecision, error) {
		return ToolDecision{}, nil
	}
	pauseSend := true
	pause := SendHook{Name: "pause", Func: func(context.Context, SendPlan) (SendDecision, error) {
		if pauseSend {
			return SendDecision{Deny: true, Reason: "later"}, nil
		}
		return SendDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{
		Client: client,
		Hooks:  JoinHooks(pause.Hooks(), toolHook("gate", noop).Hooks()),
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
		WithHooks(toolHook("prompt", noop).Hooks()),
	)
	if err != nil || out.Stop.Kind != StopRefused {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	active := out.Turn.Steps[0].ActiveHooks
	if active == nil || !slices.Equal(active.BeforeTool, []string{"gate"}) ||
		!slices.Equal(active.BeforeSend, []string{"pause"}) {
		t.Fatalf("active = %+v", active)
	}
	pauseSend = false
	runner.Hooks = pause.Hooks()
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "go"})
	if err != nil || out.Stop.Kind != StopCompleted ||
		!hasWarning(out, WarningHooksChanged) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}

func TestRequireApprovalNamesAndGates(t *testing.T) {
	named := RequireApproval("b", "a", "a").BeforeTool[0]
	if named.Name != "require_approval:a,b" {
		t.Fatalf("name = %q", named.Name)
	}
	d, err := named.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "a"}})
	if err != nil || d.Kind != DecisionNeedsApproval || d.RulesVersion != named.Name {
		t.Fatalf("named = %+v, %v", d, err)
	}
	d, _ = named.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "c"}})
	if d.Kind != "" || d.Rewrite != nil {
		t.Fatalf("other tool = %+v", d)
	}
	all := RequireApproval().BeforeTool[0]
	d, _ = all.Func(t.Context(), ToolPlan{Call: tool.Call{Name: "c"}})
	if all.Name != "require_approval:*" || d.Kind != DecisionNeedsApproval {
		t.Fatalf("all = %q %+v", all.Name, d)
	}
	joined := JoinHooks(RequireApproval("a"), RequireApproval("b"))
	if err := joined.validate(); err != nil {
		t.Fatalf("two approval hooks collide: %v", err)
	}
}

// TestHookCallOrder checks that every model call passes BeforeModel and
// BeforeSend, every eligible tool call passes BeforeTool once, and every
// final reply passes BeforeFinish, whether the run is collected or
// streamed, across a needs-approval decision, and on the reserved final
// reply.
func TestHookCallOrder(t *testing.T) {
	var order []string
	note := func(s string) {
		order = append(order, s)
	}
	hooks := Hooks{
		BeforeModel: []ModelHook{{Name: "m", Func: func(
			_ context.Context,
			p ModelPlan,
		) (ModelDecision, error) {
			note(fmt.Sprintf("model(tools=%d)", len(p.Input.Tools)))
			return ModelDecision{}, nil
		}}},
		BeforeSend: []SendHook{{Name: "s", Func: func(context.Context, SendPlan) (SendDecision, error) {
			note("send")
			return SendDecision{}, nil
		}}},
		BeforeTool: []ToolHook{{Name: "t", Func: func(
			_ context.Context,
			p ToolPlan,
		) (ToolDecision, error) {
			note("tool:" + p.Call.ID)
			if p.Call.ID == "call-2" {
				return NeedsApproval("ask"), nil
			}
			return ToolDecision{}, nil
		}}},
		BeforeFinish: []FinishHook{{Name: "f", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			note("finish")
			return FinishDecision{}, nil
		}}},
	}
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%v", streamed), func(t *testing.T) {
			order = nil
			remote := &valueTool{}
			agent := testAgent()
			agent.Limits.ModelCalls = 3
			client := &scriptedClient{results: []llm.Result{
				callResult("call-1", "remote"),
				callResult("call-2", "remote"),
				final("done"),
			}}
			runner := Runner{
				Client: client,
				Tools:  tool.List{remote.binding()},
				Hooks:  hooks,
			}
			run := func(turn Turn, command Command) RunResult {
				t.Helper()
				if !streamed {
					out, err := runner.Run(t.Context(), turn, command)
					if err != nil {
						t.Fatal(err)
					}
					return out
				}
				var out RunResult
				for event, err := range runner.Stream(t.Context(), turn, command) {
					if err != nil {
						t.Fatal(err)
					}
					if event.Result != nil {
						out = *event.Result
					}
				}
				return out
			}
			out := run(Turn{ID: "t1"}, Start{
				CommandID: "start",
				Agent:     agent,
				Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
			})
			if out.Stop.Kind != StopWaiting || len(out.Pending) != 1 {
				t.Fatalf("stop=%+v pending=%+v", out.Stop, out.Pending)
			}
			out = run(out.Turn, out.Pending[0].Approve())
			out = run(out.Turn, Continue{CommandID: "go"})
			if out.Stop.Kind != StopLimited || remote.runs != 2 {
				t.Fatalf("stop=%+v runs=%d", out.Stop, remote.runs)
			}
			want := []string{
				"model(tools=1)", "send", "tool:call-1",
				"model(tools=1)", "send", "tool:call-2",
				"model(tools=0)", "send", "finish",
			}
			if strings.Join(order, " ") != strings.Join(want, " ") {
				t.Fatalf("order = %v\nwant    %v", order, want)
			}
		})
	}
}

func TestPendingApproveNeedsOnlyCallAndArguments(t *testing.T) {
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	runner := Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  RequireApproval("remote"),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	if out.Tools["call-1"].Verdict.RulesVersion != "require_approval:remote" {
		t.Fatalf("verdict = %+v", out.Tools["call-1"].Verdict)
	}
	// The Resolve comes from a handler whose Runner has no hooks, which
	// Resolve does not need; that is not a change of hooks.
	finishr := runner
	finishr.Hooks = Hooks{}
	answer := out.Pending[0].Approve()
	if out, err = finishr.Run(t.Context(), out.Turn, answer); err != nil {
		t.Fatal(err)
	}
	stale := answer
	stale.CommandID = "stale"
	stale.Approval = new(*answer.Approval)
	stale.Approval.ArgsHash = ArgsHash(json.RawMessage(`{"value":9}`))
	if _, err := runner.Run(t.Context(), out.Turn, stale); err == nil {
		t.Fatal("an answer for other arguments was accepted")
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "go"})
	if err != nil || out.Stop.Kind != StopCompleted || remote.runs != 1 {
		t.Fatalf("stop=%+v runs=%d err=%v", out.Stop, remote.runs, err)
	}
	if hasWarning(out, WarningHooksChanged) {
		t.Fatalf("warnings = %+v", out.Warnings)
	}
}

// TestHookErrorAfterContextEndsAtEveryPoint checks that a hook failing
// because the run's context ended gives the context stop at each point, and
// that earlier hooks in the same chain leave no record.
func TestHookErrorAfterContextEndsAtEveryPoint(t *testing.T) {
	type setup func(cancel context.CancelFunc) Hooks
	ended := func(ctx context.Context, cancel context.CancelFunc) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	cases := map[string]setup{
		"before_model": func(cancel context.CancelFunc) Hooks {
			return Hooks{BeforeModel: []ModelHook{
				{Name: "a", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
					s := p.Input.Settings
					return ModelDecision{Settings: &s}, nil
				}},
				{Name: "b", Func: func(ctx context.Context, _ ModelPlan) (ModelDecision, error) {
					return ModelDecision{}, ended(ctx, cancel)
				}},
			}}
		},
		"before_send": func(cancel context.CancelFunc) Hooks {
			return Hooks{BeforeSend: []SendHook{
				{Name: "a", Func: func(context.Context, SendPlan) (SendDecision, error) {
					return SendDecision{Reservation: json.RawMessage(`{}`)}, nil
				}},
				{Name: "b", Func: func(ctx context.Context, _ SendPlan) (SendDecision, error) {
					return SendDecision{}, ended(ctx, cancel)
				}},
			}}
		},
		"before_tool": func(cancel context.CancelFunc) Hooks {
			return Hooks{BeforeTool: []ToolHook{
				{Name: "a", Func: func(context.Context, ToolPlan) (ToolDecision, error) {
					return Allow("ok"), nil
				}},
				{Name: "b", Func: func(ctx context.Context, _ ToolPlan) (ToolDecision, error) {
					return ToolDecision{}, ended(ctx, cancel)
				}},
			}}
		},
		"before_finish": func(cancel context.CancelFunc) Hooks {
			return Hooks{BeforeFinish: []FinishHook{
				{Name: "a", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
					return FinishDecision{AcceptEmpty: true}, nil
				}},
				{Name: "b", Func: func(ctx context.Context, _ FinishPlan) (FinishDecision, error) {
					return FinishDecision{}, ended(ctx, cancel)
				}},
			}}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			remote := &valueTool{}
			client := &scriptedClient{
				results: []llm.Result{callResult("call-1", "remote"), final("done")},
			}
			out, err := (Runner{
				Client: client,
				Tools:  tool.List{remote.binding()},
				Hooks:  build(cancel),
			}).Run(ctx, Turn{ID: "t1"}, Start{
				CommandID: "start",
				Agent:     testAgent(),
				Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
			})
			if err != nil || out.Stop.Kind != StopInterrupted {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			if results := allHookResults(out.Turn); len(results) != 0 {
				t.Fatalf("recorded %+v for an unfinished chain", results)
			}
		})
	}
}

func TestAddedMessagesAfterFinishFailureAsksTheModelAgain(t *testing.T) {
	fail := true
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		if fail {
			return FinishDecision{}, errors.New("guard down")
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("old"), final("nouveau")}}
	runner := Runner{Client: client, Hooks: guard.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, AddMessages{
		CommandID: "note",
		Items: []ContextItem{{
			Message: llm.UserMessage(llm.Text("answer in French")),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "nouveau" ||
		client.sends != 2 {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
	}
	last := client.inputs[len(client.inputs)-1].Messages
	if last[len(last)-1].Text() != "answer in French" {
		t.Fatalf("the model did not see the delivered context: %+v", last)
	}
}

func TestAddedMessagesAfterFinishFailureWithNoCallLeftIsDropped(t *testing.T) {
	fail := true
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		if fail {
			return FinishDecision{}, errors.New("guard down")
		}
		return FinishDecision{}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 3
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("second"),
	}}
	runner := Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  guard.Hooks(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	// The next call would be the reserved final reply, so there is no room
	// for a normal call that could use the added messages.
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, AddMessages{
		CommandID: "note",
		Items:     []ContextItem{{Message: llm.UserMessage(llm.Text("more"))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "second" ||
		client.sends != 2 || !hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v answer=%q sends=%d warnings=%+v err=%v", out.Stop, out.Reply, client.sends, out.Warnings, err)
	}
}

func TestFinishFailureOnReservedFinalReplyResumesWithoutACall(t *testing.T) {
	fail := true
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		if fail {
			return FinishDecision{}, errors.New("guard down")
		}
		return FinishDecision{}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 2
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("last"),
	}}
	runner := Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  guard.Hooks(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopLimited || out.Reply != "last" ||
		client.sends != 2 {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
	}
}

func TestHookStopCannotUseReservedReasons(t *testing.T) {
	fake := ModelHook{Name: "fake", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{Stop: &Stop{Kind: StopFailed, Reason: StopReasonHookFailed}}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, nil, fake.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || !hasWarning(out, WarningHookFailed) {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}

func TestReplayRejectsInvalidHookRecords(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := runHooked(t, client, nil, RequireApproval())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Turn){
		"unknown verdict": func(turn *Turn) {
			turn.Steps[1].HookResults = []HookResult{
				{Point: HookBeforeSend, Name: "x", Verdict: "bogus"},
			}
		},
		"verdict from another point": func(turn *Turn) {
			turn.Steps[1].HookResults = []HookResult{
				{Point: HookBeforeSend, Name: "x", Verdict: VerdictNeedsApproval},
			}
		},
		"pass outside before_tool": func(turn *Turn) {
			turn.Steps[1].HookResults = []HookResult{
				{Point: HookBeforeSend, Name: "x", Verdict: VerdictReserve, Pass: 2},
			}
		},
		"error without failure": func(turn *Turn) {
			turn.Steps[1].HookResults = []HookResult{
				{Point: HookBeforeSend, Name: "x", Verdict: VerdictReserve, Error: "e"},
			}
		},
		"failure without error": func(turn *Turn) {
			turn.Steps[1].HookResults = []HookResult{
				{Point: HookBeforeSend, Name: "x", Verdict: VerdictFailed},
			}
		},
		"duplicate active names": func(turn *Turn) {
			turn.Steps[0].ActiveHooks = &ActiveHooks{BeforeTool: []string{"a", "a"}}
		},
		"empty active name": func(turn *Turn) {
			turn.Steps[0].ActiveHooks = &ActiveHooks{BeforeTool: []string{""}}
		},
		"active hooks on a later step": func(turn *Turn) {
			turn.Steps[1].ActiveHooks = &ActiveHooks{BeforeTool: []string{"a"}}
		},
	}
	if _, err := out.Turn.View(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			turn := cloneTurn(out.Turn)
			corrupt(&turn)
			if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
				t.Fatalf("replay err = %v", err)
			}
		})
	}
}
