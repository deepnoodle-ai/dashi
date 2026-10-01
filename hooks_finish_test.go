package dashi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

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
	runner := Runner{Client: client, Hooks: hooks}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
	})
	return runner, out, err
}

func settlementOf(t *testing.T, turn Turn, callID string) Step {
	t.Helper()
	for _, step := range turn.Steps {
		if step.Kind == StepToolFinished && step.Tool.Call.ID == callID {
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
	if len(plans) != 2 || plans[0].CallID != "out-1" || plans[0].ModelCallID == "" {
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
			return FinishDecision{AddMessages: []ContextItem{note}}, nil
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
		if step.Kind == StepToolFinished {
			turn.Steps[i].Context = &ContextFact{Items: []ContextItem{{
				Message: llm.UserMessage(llm.Text("smuggled")),
			}}}
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestFinishNudgeIsDroppedWhenNoNormalCallRemains(t *testing.T) {
	nudge := FinishHook{Name: "nudge", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{AddMessages: []ContextItem{{
			Message: llm.UserMessage(llm.Text("maybe double-check")),
		}}}, nil
	}}
	agent := structuredAgent()
	agent.Limits.ModelCalls = 2
	client := &scriptedClient{results: []llm.Result{submitResult("out-1", `{"answer":1}`)}}
	_, out, err := runStructured(t, agent, client, nudge.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil ||
		!hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v output=%+v warnings=%+v err=%v", out.Stop, out.Output, out.Warnings, err)
	}
}

func TestFinishVetoUsesTheReservedCallThenStops(t *testing.T) {
	veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{Reason: "contains PII"}, nil
	}}
	t.Run("submission", func(t *testing.T) {
		agent := structuredAgent()
		agent.Limits.ModelCalls = 2
		client := &scriptedClient{results: []llm.Result{
			submitResult("out-1", `{"answer":1}`),
			submitResult("out-2", `{"answer":2}`),
		}}
		runner, out, err := runStructured(t, agent, client, veto.Hooks())
		// Call 2 is the reserved final answer; it is vetoed too.
		if err != nil || out.Stop.Kind != StopFailed ||
			out.Stop.Reason != StopReasonOutputRejected || out.Output != nil ||
			out.Stop.Terminal || client.sends != 2 {
			t.Fatalf("stop=%+v output=%+v sends=%d err=%v", out.Stop, out.Output, client.sends, err)
		}
		if seen := toolResultText(t, out.Turn, "out-2"); seen != "Not accepted: contains PII" {
			t.Fatalf("model would see %q", seen)
		}
		// No attempts remain: Continue ends the turn limited, never with a
		// vetoed output.
		out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "again"})
		if err != nil || out.Stop.Kind != StopLimited || out.Output != nil ||
			client.sends != 2 {
			t.Fatalf("final stop=%+v output=%+v err=%v", out.Stop, out.Output, err)
		}
	})
	t.Run("text", func(t *testing.T) {
		agent := testAgent()
		agent.Limits.ModelCalls = 2
		client := &scriptedClient{results: []llm.Result{final("ssn 123"), final("ssn 456")}}
		runner := Runner{Client: client, Hooks: veto.Hooks()}
		out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
		})
		if err != nil || out.Stop.Reason != StopReasonOutputRejected ||
			out.Reply != "" || client.sends != 2 {
			t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
		}
		out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "again"})
		if err != nil || out.Stop.Kind != StopLimited || out.Reply != "" {
			t.Fatalf("limited stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
	})
}

func TestFinishVetoBeforeTheReserveContinuesIntoIt(t *testing.T) {
	veto := FinishHook{Name: "pii", Func: func(_ context.Context, p FinishPlan) (FinishDecision, error) {
		if strings.Contains(p.Reply, "ssn") {
			return FinishDecision{Reason: "contains PII"}, nil
		}
		return FinishDecision{}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 3
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("ssn 123"),
		final("clean"),
	}}
	out, err := (Runner{
		Client: client,
		Tools:  tool.List{remote.binding()},
		Hooks:  veto.Hooks(),
	}).Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	})
	if err != nil || out.Reply != "clean" || client.sends != 3 ||
		out.Stop.Reason == StopReasonOutputRejected {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
	}
	if len(client.inputs[2].Tools) != 0 {
		t.Fatalf("the reserved call advertised tools: %+v", client.inputs[2].Tools)
	}
}

func TestVetoedAnswerNeverReturnsThroughAnswer(t *testing.T) {
	t.Run("interrupted", func(t *testing.T) {
		pause := make(chan struct{})
		veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			close(pause)
			return FinishDecision{Reason: "contains PII"}, nil
		}}
		client := &scriptedClient{results: []llm.Result{final("ssn 123")}}
		out, err := runHooked(t, client, nil, veto.Hooks(), WithPause(pause))
		if err != nil || out.Stop.Kind != StopInterrupted || out.Reply != "" {
			t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
	})
	t.Run("uncertain", func(t *testing.T) {
		asked := 0
		veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			asked++
			return FinishDecision{Reason: "contains PII"}, nil
		}}
		client := &scriptedClient{
			results: []llm.Result{final("ssn 123"), {}},
			errors:  []error{nil, errors.New("connection reset")},
		}
		out, err := runHooked(t, client, nil, veto.Hooks())
		if err != nil || out.Stop.Kind != StopResultUnknown || out.Reply != "" {
			t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
	})
	t.Run("a new answer clears the mark", func(t *testing.T) {
		asked := 0
		veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			asked++
			if asked == 1 {
				return FinishDecision{Reason: "contains PII"}, nil
			}
			return FinishDecision{}, nil
		}}
		client := &scriptedClient{results: []llm.Result{final("ssn 123"), final("clean")}}
		out, err := runHooked(t, client, nil, veto.Hooks())
		if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "clean" {
			t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
	})
}

func TestFinishVetoResumesAfterABudgetRaise(t *testing.T) {
	asked := 0
	veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		asked++
		if asked == 1 {
			return FinishDecision{Reason: "contains PII"}, nil
		}
		return FinishDecision{}, nil
	}}
	client := newPricedClient(final("ssn 123"), final("redacted"))
	agent := testAgent()
	agent.Settings = llm.Settings{MaxTokens: 100}
	runner := Runner{Client: client, Hooks: veto.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     agent,
		Budget:    &Budget{MaxCost: &Money{Amount: amount("0.0013"), Currency: "USD"}},
		Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
	})
	if err != nil || out.Stop.Reason != StopReasonOutputRejected || out.Reply != "" {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID: "raise",
		Budget:    &Budget{MaxCost: &Money{Amount: amount("1"), Currency: "USD"}},
	})
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "redacted" {
		t.Fatalf("resumed stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
	}
	last := client.inputs[len(client.inputs)-1].Messages
	if last[len(last)-1].Text() != "Not accepted: contains PII" {
		t.Fatalf("the model did not see the rejection: %+v", last[len(last)-1])
	}
}

func TestFinishReasonAloneRejectsText(t *testing.T) {
	asked := 0
	veto := FinishHook{Name: "pii", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		asked++
		if asked == 1 {
			return FinishDecision{Reason: "contains PII"}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("ssn 123"), final("redacted")}}
	out, err := runHooked(t, client, nil, veto.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "redacted" ||
		client.sends != 2 {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
	}
	last := client.inputs[1].Messages
	if last[len(last)-1].Text() != "Not accepted: contains PII" {
		t.Fatalf("second request tail = %+v", last[len(last)-1])
	}
}

func TestFinishReasonIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	veto := FinishHook{Name: "long", Func: func(_ context.Context, p FinishPlan) (FinishDecision, error) {
		if p.CallID == "out-1" {
			return FinishDecision{Reason: long}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":1}`),
		submitResult("out-2", `{"answer":2}`),
	}}
	_, out, err := runStructured(t, structuredAgent(), client, veto.Hooks())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if seen := toolResultText(t, out.Turn, "out-1"); len(seen) != len("Not accepted: ")+maxHookReason {
		t.Fatalf("model saw %d bytes", len(seen))
	}
}

func TestSubmissionRejectionSurvivesCrashes(t *testing.T) {
	for _, kind := range []StepKind{StepToolFinished, StepModelCallStarted} {
		t.Run(string(kind), func(t *testing.T) {
			asked := 0
			guard := FinishHook{Name: "tests", Func: func(
				_ context.Context,
				p FinishPlan,
			) (FinishDecision, error) {
				asked++
				if p.CallID == "out-1" {
					return FinishDecision{AddMessages: []ContextItem{{
						Origin:  ContextRuntime,
						Message: llm.UserMessage(llm.Text("ITEM")),
					}}}, nil
				}
				return FinishDecision{}, nil
			}}
			client := &scriptedClient{results: []llm.Result{
				submitResult("out-1", `{"answer":1}`),
				submitResult("out-2", `{"answer":2}`),
			}}
			base := NewMemoryRecorder()
			// The first model intent is the first call; crash on the second.
			rec := &failNthKind{base: base, kind: kind, n: 1}
			if kind == StepModelCallStarted {
				rec.n = 2
			}
			runner := Runner{
				Client:   client,
				Recorder: rec,
				Hooks:    guard.Hooks(),
			}
			_, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
				CommandID: "start",
				Agent:     structuredAgent(),
				Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
			})
			if persistence(err) != PersistenceRejected {
				t.Fatalf("err = %v", err)
			}
			saved, err := base.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			out, err := runner.Run(t.Context(), saved, Continue{CommandID: "recover"})
			if err != nil || out.Stop.Kind != StopCompleted ||
				string(out.Output.Value) != `{"answer":2}` {
				t.Fatalf("stop=%+v output=%+v err=%v", out.Stop, out.Output, err)
			}
			rejections := 0
			for _, step := range out.Turn.Steps {
				if step.Kind == StepToolFinished && step.Tool.Call.ID == "out-1" {
					rejections++
				}
			}
			sent := client.inputs[len(client.inputs)-1].Messages
			items := 0
			for _, m := range sent {
				if m.Text() == "ITEM" {
					items++
				}
			}
			if rejections != 1 || items != 1 {
				t.Fatalf("rejections=%d items seen=%d", rejections, items)
			}
		})
	}
}

// failNthKind rejects the n-th commit of one step kind.
type failNthKind struct {
	base *MemoryRecorder
	kind StepKind
	n    int
	seen int
}

func (f *failNthKind) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == f.kind {
		f.seen++
		if f.seen == f.n {
			return &CommitError{
				Persistence: PersistenceRejected,
				Err:         errors.New("injected failure"),
			}
		}
	}
	return f.base.Commit(ctx, req)
}

func (f *failNthKind) Load(ctx context.Context, id string) (Turn, error) {
	return f.base.Load(ctx, id)
}

func TestReplayRejectsContextOnASchemaRejection(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":"x"}`),
		submitResult("out-2", `{"answer":2}`),
	}}
	_, out, err := runStructured(t, structuredAgent(), client, Hooks{})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	turn := cloneTurn(out.Turn)
	for i, step := range turn.Steps {
		if step.Kind == StepToolFinished && step.Tool.Call.ID == "out-1" {
			turn.Steps[i].Context = &ContextFact{Items: []ContextItem{{
				Message: llm.UserMessage(llm.Text("smuggled")),
			}}}
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestFinishTextRejectedOnCallBeforeReserveIsDropped(t *testing.T) {
	always := FinishHook{Name: "more", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{AddMessages: []ContextItem{{
			Message: llm.UserMessage(llm.Text("again")),
		}}}, nil
	}}
	agent := testAgent()
	agent.Limits.ModelCalls = 3
	remote := &valueTool{}
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "remote"),
		final("second"),
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
	// Call 3 would be the reserved final answer, so the candidate from
	// call 2 cannot be turned down.
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "second" ||
		client.sends != 2 || !hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Reply, client.sends, err)
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
	out, err = runner.Run(t.Context(), out.Turn, AddMessages{
		CommandID: "note",
		Items:     []ContextItem{{Message: llm.UserMessage(llm.Text("use 3"))}},
	})
	if err != nil || hasWarning(out, WarningMessagesDropped) {
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
			return FinishDecision{AddMessages: []ContextItem{{
				Message: llm.UserMessage(llm.Text("not yet")),
			}}}, nil
		}
		return FinishDecision{}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("draft"), final("fixed")}}
	runner := Runner{Client: client, Hooks: guard.Hooks()}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}, WithPause(pause))
	if err != nil || out.Stop.Kind != StopInterrupted || client.sends != 1 ||
		hasWarning(out, WarningMessagesDropped) {
		t.Fatalf("stop=%+v sends=%d warnings=%+v err=%v", out.Stop, client.sends, out.Warnings, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "fixed" {
		t.Fatalf("resumed stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
	}
}

// textAndSubmission is one model result with text beside a submission.
func textAndSubmission(id, text, args string) llm.Result {
	result := submitResult(id, args)
	result.Message.Content = append(
		[]llm.Content{llm.Text(text)},
		result.Message.Content...,
	)
	return result
}

func TestVetoedSubmissionHidesItsMessageText(t *testing.T) {
	veto := FinishHook{Name: "pii", Func: func(_ context.Context, p FinishPlan) (FinishDecision, error) {
		if strings.Contains(p.Reply, "ssn") {
			return FinishDecision{Reason: "contains PII"}, nil
		}
		return FinishDecision{}, nil
	}}
	t.Run("output_rejected", func(t *testing.T) {
		agent := structuredAgent()
		agent.Limits.ModelCalls = 1
		client := &scriptedClient{results: []llm.Result{
			textAndSubmission("out-1", "ssn 123", `{"answer":1}`),
		}}
		runner, out, err := runStructured(t, agent, client, veto.Hooks())
		if err != nil || out.Stop.Reason != StopReasonOutputRejected ||
			out.Reply != "" || out.Output != nil {
			t.Fatalf("stop=%+v answer=%q output=%+v err=%v", out.Stop, out.Reply, out.Output, err)
		}
		out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "again"})
		if err != nil || out.Stop.Kind != StopLimited || out.Reply != "" {
			t.Fatalf("limited stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
		if _, err := Replay(out.Turn); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a new result clears it", func(t *testing.T) {
		client := &scriptedClient{results: []llm.Result{
			textAndSubmission("out-1", "ssn 123", `{"answer":1}`),
			textAndSubmission("out-2", "clean", `{"answer":2}`),
		}}
		_, out, err := runStructured(t, structuredAgent(), client, veto.Hooks())
		if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "clean" {
			t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
		}
	})
}

func TestBoundedReasonStaysWithinTheLimit(t *testing.T) {
	bad := string([]byte{0xff})
	for name, reason := range map[string]string{
		"invalid bytes near the limit":   strings.Repeat("x", maxHookReason-2) + bad + bad + bad,
		"invalid bytes inside the limit": strings.Repeat(bad, maxHookReason),
		"a rune across the limit":        strings.Repeat("x", maxHookReason-1) + "é",
		"short":                          "ok" + bad,
	} {
		got := boundedReason(reason)
		if len(got) > maxHookReason || !utf8.ValidString(got) {
			t.Fatalf("%s: %d bytes, valid=%v", name, len(got), utf8.ValidString(got))
		}
	}
}
