package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func limitedForModelCalls(stop Stop) bool {
	return stop.Kind == StopLimited && stop.Reason == "model_calls" &&
		stop.Limit != nil && stop.Limit.Kind == LimitModelCalls
}

func TestReservedFinalReplyUsesLastModelCallWithoutExtraCall(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelCalls: 2}
	def.Settings.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAny}
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			final("Found 42; further work is unfinished."),
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
				Output: new(tool.TextOutput("42")),
			}, nil
		}),
	}
	rec := NewMemoryRecorder()
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || !limitedForModelCalls(out.Stop) ||
		out.Reply != "Found 42; further work is unfinished." || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v err=%v sends=%d runs=%d", out, err, client.sends, runs)
	}
	if len(client.inputs) != 2 || len(client.inputs[0].Tools) != 1 || len(client.inputs[1].Tools) != 0 ||
		client.inputs[1].Settings.ToolChoice != nil {
		t.Fatalf("model inputs=%+v", client.inputs)
	}
	if client.inputs[1].System == "" || client.inputs[1].System == client.inputs[0].System {
		t.Fatal("final-answer instruction was not sent")
	}
	callStarts := 0
	for _, step := range out.Turn.Steps {
		if step.Kind != StepModelCallStarted {
			continue
		}
		callStarts++
		if step.Model.FinalReplyOnly != (callStarts == 2) || (callStarts == 2 && len(step.Model.Tools) != 0) {
			t.Fatalf("call start %d=%+v", callStarts, step.Model)
		}
	}
	view, err := out.Turn.View()
	if err != nil || view.ModelCalls != 2 || view.Stop.Kind != StopLimited {
		t.Fatalf("replay=%+v err=%v", view, err)
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
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || again.Stop.Kind != StopLimited || client.sends != 2 || runs != 1 {
		t.Fatalf(
			"duplicate start=%+v err=%v sends=%d runs=%d",
			again,
			err,
			client.sends,
			runs,
		)
	}
	if _, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(t.Context(), out.Turn, Continue{CommandID: "too-late"}); err == nil {
		t.Fatal("exhausted turn accepted a continuation")
	}
}

func TestReservedFinalReplyKeepsStructuredOutputSubmission(t *testing.T) {
	def := structuredAgent()
	def.Limits.ModelCalls = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	binding := answerLookup()
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
		},
	)
	if err != nil || !limitedForModelCalls(out.Stop) ||
		out.Output == nil || out.Output.Source != OutputSourceToolCall || string(out.Output.Value) != `{"answer":42}` || client.sends != 2 {
		t.Fatalf("out=%+v err=%v sends=%d", out, err, client.sends)
	}
	if len(client.inputs[1].Tools) != 1 || client.inputs[1].Tools[0].Name != SubmitOutputTool {
		t.Fatalf("final tools=%+v", client.inputs[1].Tools)
	}
	if !json.Valid(out.Output.Value) {
		t.Fatalf("structured output=%+v", out.Output)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallStarted && step.Model.FinalReplyOnly {
			step.Model.Tools = append(step.Model.Tools, binding.Definition)
			if _, err := out.Turn.View(); err == nil {
				t.Fatal("replay accepted an application tool on the final-answer request")
			}
			return
		}
	}
	t.Fatal("missing final-reply call start")
}

func TestReplayAllowsDeadlineAfterRecordedOutput(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{submitResult("submission", `{"answer":8}`)},
	}
	out := applyStructured(t, client, nil, NewMemoryRecorder())
	i := slices.IndexFunc(out.Turn.Steps, func(step Step) bool {
		return step.Kind == StepOutputRecorded
	})
	if i < 0 {
		t.Fatal("no recorded output")
	}
	recorded := out.Turn.Steps[i]

	tests := []struct {
		name    string
		stop    Stop
		allowed bool
	}{
		{
			name: "model calls without final attempt",
			stop: *testStop(StopLimited, LimitModelCalls),
		},
		{
			name:    "deadline",
			stop:    *testStop(StopLimited, LimitDeadline),
			allowed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := tt.stop
			turn := out.Turn
			turn.Steps = append(
				slices.Clone(out.Turn.Steps[:i+1]),
				Step{
					Version: recorded.Version,
					TurnID:  recorded.TurnID,
					Seq:     int64(i + 2),
					Kind:    StepTurnStopped,
					Stop:    &stop,
				},
			)
			_, err := turn.View()
			if (err == nil) != tt.allowed {
				t.Fatalf("replay of %+v: allowed=%t err=%v", stop, tt.allowed, err)
			}
		})
	}

	def := structuredAgent()
	def.Limits.ModelCalls = 2
	final := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	start := Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
	}
	runner := Runner{
		Client:   final,
		Tools:    tool.List{answerLookup()},
		Recorder: NewMemoryRecorder(),
	}
	limited, err := runner.Run(
		t.Context(),
		Turn{ID: "limit"},
		start,
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := limited.Turn.View()
	if err != nil {
		t.Fatalf("replay rejected limited stop after final attempt: %v", err)
	}
	if !limitedForModelCalls(view.Stop) {
		t.Fatalf("stop=%+v", view.Stop)
	}
}

func TestReservedFinalReplyCannotExecuteUnadvertisedTool(t *testing.T) {
	def := testAgent()
	def.Limits.ModelCalls = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("first", "lookup"),
			callResult("second", "lookup"),
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
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up"))},
		},
	)
	if err != nil || out.Stop.Kind != StopLimited || client.sends != 2 || runs != 1 {
		t.Fatalf("out=%+v err=%v sends=%d runs=%d", out, err, client.sends, runs)
	}
	view, err := out.Turn.View()
	if err != nil || view.Tools["second"].Status != ToolFinished ||
		view.Tools["second"].Outcome == nil || view.Tools["second"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("replay=%+v err=%v", view, err)
	}
}

func TestReservedFinalReplyClassifiesAfterStopWriteFailure(t *testing.T) {
	def := testAgent()
	def.Limits.ModelCalls = 2
	client := &scriptedClient{
		results: []llm.Result{callResult("first", "lookup"), final("42")},
	}
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("42")),
			}, nil
		}),
	}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepTurnStopped}
	first, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "limit"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("look up"))},
		},
	)
	if !nothingWritten(err) || client.sends != 2 {
		t.Fatalf("first=%+v err=%v sends=%d", first, err, client.sends)
	}
	saved, err := base.Load(t.Context(), "limit")
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(t.Context(), saved, Continue{CommandID: "recover"})
	if err != nil || !limitedForModelCalls(second.Stop) ||
		second.Reply != "42" || client.sends != 2 {
		t.Fatalf("recovered=%+v err=%v sends=%d", second, err, client.sends)
	}
}

// answerLookup is a local tool that always returns 42.
func answerLookup() tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("42"))}, nil
		}),
	}
}

func TestContinueRaisesModelCallLimit(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelCalls: 2, NoReservedFinalReply: true}
	client := &scriptedClient{
		results: []llm.Result{
			callResult("c1", "lookup"),
			callResult("c2", "lookup"),
			callResult("c3", "lookup"),
			final("done"),
		},
	}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		}),
	}
	runner := Runner{
		Client:   client,
		Tools:    tool.List{lookup},
		Recorder: NewMemoryRecorder(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "limit"}, Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil || !limitedForModelCalls(out.Stop) || out.Stop.Ended {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}

	// Without a raise, or with a raise that does not exceed the limit, the
	// turn does not continue.
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID: "plain",
	}); err == nil {
		t.Fatal("continue without a raise ran a spent turn")
	}
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "same",
		ModelCalls: 2,
	}); err == nil {
		t.Fatal("a raise equal to the limit was accepted")
	}

	// The raise keeps the same turn and its record.
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "raise-1",
		ModelCalls: 3,
	})
	if err != nil || !limitedForModelCalls(out.Stop) || out.Turn.ID != "limit" {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if out.Stop.Limit.Limit != 3 || out.Stop.Limit.Used != 3 {
		t.Fatalf("limit detail=%+v", out.Stop.Limit)
	}

	// A second raise builds on the first, and the record still builds a view.
	if _, err := runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "lower",
		ModelCalls: 3,
	}); err == nil {
		t.Fatal("a raise equal to the raised limit was accepted")
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "raise-2",
		ModelCalls: 10,
	})
	if err != nil || out.Stop.Kind != StopCompleted || out.Reply != "done" {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Reply, err)
	}
	if _, err := out.Turn.View(); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestBudgetRaiseAloneCannotRunToolsWithoutModelCalls(t *testing.T) {
	s := &replayState{}
	s.LastSeq, s.ModelCalls, s.modelCallCap = 2, 3, 3
	s.Stop = Stop{
		Kind:  StopLimited,
		Limit: &LimitDetail{Kind: LimitTotalTokens},
	}
	s.Tools = map[string]ToolView{"c1": {Status: ToolRequested}}
	s.callOrder = []string{"c1"}
	raise := &Budget{MaxTotalTokens: 1 << 40}
	if s.canContinueWith(raise, 0) || s.canContinueWith(raise, 3) {
		t.Fatal("a budget raise without a model-call raise was accepted")
	}
	if !s.canContinueWith(raise, 4) {
		t.Fatal("a budget raise with room for a model call was rejected")
	}
}

func TestStructuredAnswerOnFinalCallCompletesAfterContinue(t *testing.T) {
	def := structuredAgent()
	def.Limits.ModelCalls = 2
	client := &scriptedClient{
		results: []llm.Result{
			callResult("lookup-1", "lookup"),
			submitResult("submit", `{"answer":42}`),
		},
	}
	runner := Runner{
		Client:   client,
		Tools:    tool.List{answerLookup()},
		Recorder: NewMemoryRecorder(),
	}
	out, err := runner.Run(t.Context(), Turn{ID: "limit"}, Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("look up 42"))},
	})
	if err != nil || !limitedForModelCalls(out.Stop) {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "more",
		ModelCalls: 3,
	})
	if err != nil || out.Stop.Kind != StopCompleted || out.Output == nil ||
		client.sends != 2 {
		t.Fatalf("stop=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
}

func TestRunnerTimeoutRetainsOrderedMixedPartialData(t *testing.T) {
	reasoning := llm.Content{
		Kind:      llm.ContentReasoning,
		Reasoning: &llm.Reasoning{Text: "thought"},
	}
	textC := llm.Text("C")
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors: []error{&llm.Error{
			Provider: "test", Category: llm.ErrorTransport,
			Reason: llm.ReasonStreamIdleTimeout,
		}},
		deltas: []llm.Delta{
			{Kind: llm.DeltaText, Index: 0, Text: "A"},
			{Kind: llm.DeltaContent, Index: 1, Content: &reasoning},
			{Kind: llm.DeltaText, Index: 2, Text: "B"},
			// A snapshot takes priority if a client also supplies Text.
			{Kind: llm.DeltaText, Index: 3, Text: "duplicate", Content: &textC},
			{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 5, OutputTokens: 1}},
			{Kind: llm.DeltaUsage, Usage: &llm.Usage{OutputTokens: 2}},
		},
	}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil || out.Stop.Kind != StopResultUnknown || out.Reply != "" {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	var finished *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished {
			finished = step.Model
		}
	}
	if finished == nil || finished.ErrorReason != llm.ReasonStreamIdleTimeout ||
		finished.SendStatus != llm.SendStatusUnknown || !finished.ResultUnknown ||
		!finished.EstimatedUsage || finished.Result == nil ||
		finished.Result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("finish=%+v", finished)
	}
	parts := finished.Result.Message.Content
	if len(parts) != 4 || parts[0].Block.Text.Text != "A" ||
		parts[1].Reasoning.Text != "thought" ||
		parts[2].Block.Text.Text != "B" ||
		parts[3].Block.Text.Text != "C" ||
		finished.Result.Message.Text() != "ABC" {
		t.Fatalf("observed content=%+v", parts)
	}
	if finished.Result.Usage == nil || finished.Result.Usage.InputTokens != 5 ||
		finished.Result.Usage.OutputTokens != 2 || out.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("usage=%+v spend=%+v", finished.Result.Usage, out.Spend)
	}
	replayed, err := out.Turn.View()
	if err != nil || replayed.Reply != "" || replayed.Stop.Kind != StopResultUnknown {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
}

type timeoutThenCancelClient struct {
	*scriptedClient
	cancel context.CancelFunc
}

func (c timeoutThenCancelClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	result, err := c.scriptedClient.Send(ctx, call)
	c.cancel()
	return result, err
}

func TestRunnerKeepsProviderTimeoutWhenHostCancelsLater(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := timeoutThenCancelClient{
		scriptedClient: &scriptedClient{
			results: []llm.Result{{}},
			errors: []error{&llm.Error{
				Provider: "test",
				Category: llm.ErrorTransport,
				Reason:   llm.ReasonStreamIdleTimeout,
			}},
		},
		cancel: cancel,
	}
	out, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(ctx, Turn{ID: "t1"}, streamStart())
	if err != nil || out.Stop.Kind != StopResultUnknown {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished {
			if step.Model.Interrupted != InterruptNone ||
				step.Model.ErrorReason != llm.ReasonStreamIdleTimeout {
				t.Fatalf("provider timeout lost to later host cancel: %+v", step.Model)
			}
			return
		}
	}
	t.Fatal("missing model call result")
}

// identityClient records the call identity each Send receives.
type identityClient struct {
	*scriptedClient
	sent   []llm.CallIdentity
	onSend func(llm.CallIdentity)
}

func (c *identityClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	if call.Identity.IsZero() {
		return llm.Result{}, errors.New("send without attempt identity")
	}
	c.sent = append(c.sent, call.Identity)
	if c.onSend != nil {
		c.onSend(call.Identity)
	}
	return c.scriptedClient.Send(ctx, call)
}

func stepModelCalls(turn Turn, kind StepKind) []string {
	var ids []string
	for _, step := range turn.Steps {
		if step.Kind == kind {
			ids = append(ids, step.Model.ModelCallID)
		}
	}
	return ids
}

func modelCallIDs(t *testing.T, modelCalls []llm.CallIdentity) []string {
	t.Helper()
	ids := make([]string, len(modelCalls))
	for i, a := range modelCalls {
		if a.TurnID != "t1" {
			t.Fatalf("attempt %d turn=%q, want t1", i, a.TurnID)
		}
		ids[i] = a.ModelCallID
	}
	return ids
}

func TestClientReceivesRecordedCallIdentity(t *testing.T) {
	rec := NewMemoryRecorder()
	client := &identityClient{
		scriptedClient: &scriptedClient{results: []llm.Result{final("done")}},
	}
	client.onSend = func(a llm.CallIdentity) {
		// The model_call_started step carrying this ID is saved before Send
		// is called.
		saved, err := rec.Load(t.Context(), "t1")
		if err != nil {
			t.Fatal(err)
		}
		last := saved.Steps[len(saved.Steps)-1]
		if last.Kind != StepModelCallStarted || last.Model.ModelCallID != a.ModelCallID {
			t.Fatalf(
				"send %q before its call start was recorded: last step %s",
				a.ModelCallID,
				last.Kind,
			)
		}
	}
	out, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	callStarts := stepModelCalls(out.Turn, StepModelCallStarted)
	if want := []string{"t1:model:1"}; !slices.Equal(callStarts, want) {
		t.Fatalf("call starts=%v", callStarts)
	}
	if got := modelCallIDs(t, client.sent); !slices.Equal(got, callStarts) {
		t.Fatalf("sent=%v call starts=%v", got, callStarts)
	}
}

func TestInterruptedModelCallIsResentUnderNewID(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &identityClient{
		scriptedClient: &scriptedClient{results: []llm.Result{{}, final("done")}},
	}
	client.onSend = func(llm.CallIdentity) {
		if len(client.sent) == 1 {
			cancel(errors.New("deploying"))
		}
	}
	first, err := (Runner{Client: client, Recorder: rec}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: def, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("first stop=%+v err=%v", first.Stop, err)
	}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue"},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("continue stop=%+v err=%v", out.Stop, err)
	}
	callStarts := stepModelCalls(out.Turn, StepModelCallStarted)
	if want := []string{
		"t1:model:1",
		"t1:model:2",
	}; !slices.Equal(callStarts, want) {
		t.Fatalf("call starts=%v", callStarts)
	}
	if got := modelCallIDs(t, client.sent); !slices.Equal(got, callStarts) {
		t.Fatalf("sent=%v call starts=%v", got, callStarts)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished &&
			step.Model.ModelCallID == "t1:model:1" &&
			step.Model.Interrupted == InterruptNone {
			t.Fatalf("first attempt finished without interruption: %+v", step.Model)
		}
	}
}

func TestCrashedCallStartIsNeverSentUnderItsID(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	turn := seedOpenModelCallStart(t, rec, def, nil)
	client := &identityClient{
		scriptedClient: &scriptedClient{results: []llm.Result{final("done")}},
	}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "recover"},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got, want := modelCallIDs(t, client.sent), []string{
		"t1:model:2",
	}; !slices.Equal(got, want) {
		t.Fatalf("sent=%v, want %v", got, want)
	}
	finished := stepModelCalls(out.Turn, StepModelCallFinished)
	if want := []string{
		"t1:model:1",
		"t1:model:2",
	}; !slices.Equal(finished, want) {
		t.Fatalf("finished=%v", finished)
	}
}
