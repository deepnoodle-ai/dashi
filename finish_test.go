package dashi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func runStructured(
	t *testing.T,
	agent Agent,
	client llm.Client,
	hooks Hooks,
) (Runner, Outcome, error) {
	t.Helper()
	runner := Runner{Agent: agent, Client: client, Hooks: hooks}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
	})
	return runner, out, err
}

func settlementOf(t *testing.T, turn Turn, callID string) Step {
	t.Helper()
	for _, step := range turn.Steps {
		if step.Kind == StepToolSettled && step.Tool.Call.ID == callID {
			return step
		}
	}
	t.Fatalf("no settlement for %s", callID)
	return Step{}
}

func TestFinishRejectsSubmissionWithReason(t *testing.T) {
	var plans []FinishPlan
	guard := FinishHook{Name: "guard", Func: func(
		_ context.Context,
		p FinishPlan,
	) (FinishDecision, error) {
		plans = append(plans, p)
		if string(p.Output) == `{"answer":1}` {
			return FinishDecision{Reason: "the answer must be 2"}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":1}`),
		submitResult("out-2", `{"answer":2}`),
	}}
	_, out, err := runStructured(t, structuredAgent(), client, guard.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted ||
		string(out.Output.Value) != `{"answer":2}` || out.Output.ToolCallID != "out-2" {
		t.Fatalf("stop=%+v output=%+v err=%v", out.Stop, out.Output, err)
	}
	if len(plans) != 2 || plans[0].CallID != "out-1" || plans[0].AttemptID == "" {
		t.Fatalf("plans = %+v", plans)
	}
	if seen := toolResultText(t, out.Turn, "out-1"); seen != "Not accepted: the answer must be 2" {
		t.Fatalf("model saw %q", seen)
	}
	rejected := settlementOf(t, out.Turn, "out-1")
	if rejected.Tool.Outcome.State != tool.StateFailed ||
		rejected.Tool.Reason != "not accepted: hook guard" ||
		len(rejected.HookResults) != 1 ||
		rejected.HookResults[0].Verdict != VerdictReject ||
		rejected.HookResults[0].Reason != "the answer must be 2" {
		t.Fatalf("rejected step = %+v", rejected)
	}
}

func TestFinishRejectsSubmissionWithItemsOnTheSameStep(t *testing.T) {
	note := ContextItem{
		Origin:  ContextRuntime,
		Message: llm.UserMessage(llm.Text("tests fail")),
	}
	guard := FinishHook{Name: "tests", Func: func(
		_ context.Context,
		p FinishPlan,
	) (FinishDecision, error) {
		if p.CallID == "out-1" {
			return FinishDecision{Deliver: []ContextItem{note}}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":1}`),
		submitResult("out-2", `{"answer":2}`),
	}}
	_, out, err := runStructured(t, structuredAgent(), client, guard.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	rejected := settlementOf(t, out.Turn, "out-1")
	if rejected.Context == nil || len(rejected.Context.Items) != 1 {
		t.Fatalf("items not on the settlement: %+v", rejected)
	}
	if seen := toolResultText(t, out.Turn, "out-1"); seen != "Not accepted: a check did not accept this output" {
		t.Fatalf("model saw %q", seen)
	}
	// The second call saw the result, then the item.
	sent := client.inputs[1].Messages
	if sent[len(sent)-1].Text() != "tests fail" ||
		sent[len(sent)-2].Content[0].Kind != llm.ContentToolResult {
		t.Fatalf("second request tail = %+v", sent[len(sent)-2:])
	}
	if _, err := Replay(out.Turn); err != nil {
		t.Fatal(err)
	}
}

func TestReplayAllowsContextOnlyOnATurnedDownSubmission(t *testing.T) {
	remote := &valueTool{}
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "remote"), final("done")},
	}
	out, err := runHooked(t, client, tool.List{remote.binding()}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	turn := cloneTurn(out.Turn)
	for i, step := range turn.Steps {
		if step.Kind == StepToolSettled {
			turn.Steps[i].Context = &ContextFact{Items: []ContextItem{{
				Message: llm.UserMessage(llm.Text("smuggled")),
			}}}
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestFinishKeepsSubmissionWhenNoNormalCallRemains(t *testing.T) {
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{Reason: "try again"}, nil
	}}
	agent := structuredAgent()
	agent.Limits.ModelAttempts = 2
	client := &scriptedClient{results: []llm.Result{submitResult("out-1", `{"answer":1}`)}}
	_, out, err := runStructured(t, agent, client, guard.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil ||
		!hasWarning(out, WarningDeliveryDropped) {
		t.Fatalf("stop=%+v output=%+v warnings=%+v err=%v", out.Stop, out.Output, out.Warnings, err)
	}
}

func TestFinishTextRejectedOnCallBeforeReserveIsDropped(t *testing.T) {
	always := FinishHook{Name: "more", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{Deliver: []ContextItem{{
			Message: llm.UserMessage(llm.Text("again")),
		}}}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelAttempts = 3
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("second"),
	}}
	out, err := (Runner{
		Agent:  agent,
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  always.Hooks(),
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	// Call 3 would be the reserved final answer, so the candidate from
	// call 2 cannot be turned down.
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "second" ||
		client.sends != 2 || !hasWarning(out, WarningDeliveryDropped) {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Answer, client.sends, err)
	}
}

func TestFinishFailureOnSubmissionRetriesWithoutACall(t *testing.T) {
	fail := true
	asked := 0
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		asked++
		if fail {
			return FinishDecision{}, errors.New("guard down")
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{submitResult("out-1", `{"answer":1}`)}}
	runner, out, err := runStructured(t, structuredAgent(), client, guard.Hooks())
	if err != nil || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if out.Tools["out-1"].Status != ToolStarted {
		t.Fatalf("submission status = %s", out.Tools["out-1"].Status)
	}
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 ||
		asked != 2 || out.Output == nil {
		t.Fatalf("stop=%+v sends=%d asked=%d err=%v", out.Stop, client.sends, asked, err)
	}
}

func TestDeliveryAfterSubmissionFailureAsksTheModelAgain(t *testing.T) {
	fail := true
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		if fail {
			return FinishDecision{}, errors.New("guard down")
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":1}`),
		submitResult("out-2", `{"answer":3}`),
	}}
	runner, out, err := runStructured(t, structuredAgent(), client, guard.Hooks())
	if err != nil || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	fail = false
	out, err = runner.Run(t.Context(), out.Turn, Deliver{
		CommandID: "note",
		Items:     []ContextItem{{Message: llm.UserMessage(llm.Text("use 3"))}},
	})
	if err != nil || hasWarning(out, WarningDeliveryDropped) {
		t.Fatalf("warnings=%+v err=%v", out.Warnings, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "retry"})
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 2 ||
		string(out.Output.Value) != `{"answer":3}` {
		t.Fatalf("stop=%+v output=%+v sends=%d err=%v", out.Stop, out.Output, client.sends, err)
	}
	if !strings.HasPrefix(toolResultText(t, out.Turn, "out-1"), "Not accepted: new context") {
		t.Fatalf("model saw %q", toolResultText(t, out.Turn, "out-1"))
	}
}

func TestPauseDuringFinishKeepsTheRejection(t *testing.T) {
	pause := make(chan struct{})
	asked := 0
	guard := FinishHook{Name: "guard", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		asked++
		if asked == 1 {
			close(pause)
			return FinishDecision{Deliver: []ContextItem{{
				Message: llm.UserMessage(llm.Text("not yet")),
			}}}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("draft"), final("fixed")}}
	runner := Runner{Agent: testAgent(), Client: client, Hooks: guard.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}, WithPause(pause))
	if err != nil || out.Stop.Kind != StopInterrupted || client.sends != 1 ||
		hasWarning(out, WarningDeliveryDropped) {
		t.Fatalf("stop=%+v sends=%d warnings=%+v err=%v", out.Stop, client.sends, out.Warnings, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "fixed" {
		t.Fatalf("resumed stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
	}
}
