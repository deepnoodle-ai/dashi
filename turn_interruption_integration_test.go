package kernel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type interruptionClient struct {
	*scriptedClient
	onSend func(context.Context, llm.Sink, int) (llm.Result, error)
}

func (c *interruptionClient) Send(
	ctx context.Context,
	_ llm.Request,
	sink llm.Sink,
) (llm.Result, error) {
	index := c.sends
	c.sends++
	return c.onSend(ctx, sink, index)
}

func TestRepeatedModelInterruptionsKeepUsageAndAttemptAllowance(
	t *testing.T,
) {
	rec := NewMemoryRecorder()
	def := definition()
	def.Limits.Interruptions = 2
	var cancel context.CancelCauseFunc
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, sink llm.Sink, _ int) (
			llm.Result, error,
		) {
			sink(llm.Delta{Kind: llm.DeltaText, Text: "partial"})
			sink(llm.Delta{
				Kind:  llm.DeltaUsage,
				Usage: &llm.Usage{InputTokens: 2, OutputTokens: 1},
			})
			cancel(errors.New("deploying"))
			return llm.Result{
				Message:   llm.AssistantMessage(llm.Text("partial")),
				Usage:     &llm.Usage{InputTokens: 2, OutputTokens: 1},
				Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
			}, ctx.Err()
		},
	}
	turn := Turn{ID: "t1"}
	for i := 0; i < 3; i++ {
		ctx, stop := context.WithCancelCause(context.Background())
		cancel = stop
		var command Command = Continue{ID: "continue-" + string(rune('a'+i))}
		if i == 0 {
			command = Start{
				ID:    "start",
				Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
			}
		}
		out, err := (Engine{}).Apply(
			ctx, turn, def, command,
			Bindings{Client: client, Recorder: rec},
		)
		if err != nil || out.Stop.Kind != StopInterrupted ||
			out.Stop.Reason != "deploying" {
			t.Fatalf("interruption %d: stop=%+v err=%v", i, out.Stop, err)
		}
		turn = out.Turn
		view, err := Replay(turn)
		if err != nil {
			t.Fatal(err)
		}
		if view.ModelAttempts != 0 || view.Interruptions != i ||
			view.Spend.Usage.InputTokens != 2*(i+1) ||
			view.Spend.Usage.OutputTokens != i+1 {
			t.Fatalf("interruption %d: view=%+v", i, view)
		}
		if out.ReadyToContinue != (i < def.Limits.Interruptions) ||
			view.ReadyToContinue != out.ReadyToContinue ||
			turn.AnswerText() != "" {
			t.Fatalf("interruption %d: outcome=%+v view=%+v", i, out, view)
		}
	}
	limited, err := (Engine{}).Apply(
		context.Background(), turn, def, Continue{ID: "over-limit"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || limited.Stop.Kind != StopLimited ||
		limited.Stop.Reason != "interruptions" || client.sends != 3 {
		t.Fatalf("limit=%+v err=%v sends=%d", limited.Stop, err, client.sends)
	}
	view, err := Replay(limited.Turn)
	if err != nil || view.ModelAttempts != 0 || view.Interruptions != 2 ||
		view.Spend.Usage.InputTokens != 6 ||
		view.ReadyToContinue || limited.ReadyToContinue {
		t.Fatalf("limited view=%+v err=%v", view, err)
	}
	last := limited.Turn.Steps[len(limited.Turn.Steps)-1]
	if last.Kind != StepStopped || last.CommandID != "over-limit" {
		t.Fatalf("excess continue ran work: %+v", last)
	}
	for _, step := range limited.Turn.Steps {
		if step.Kind == StepModelSettled && !step.Model.Interrupted {
			t.Fatalf("model settlement was actionable: %+v", step.Model)
		}
	}
}

func TestInterruptedModelKeepsDeltaOnlyPartialOutputAndUsage(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, sink llm.Sink, _ int) (
			llm.Result, error,
		) {
			sink(llm.Delta{Kind: llm.DeltaText, Text: "part"})
			sink(llm.Delta{Kind: llm.DeltaText, Text: "ial"})
			sink(llm.Delta{
				Kind:  llm.DeltaUsage,
				Usage: &llm.Usage{InputTokens: 5},
			})
			cancel(errors.New("worker stopped"))
			return llm.Result{}, ctx.Err()
		},
	}
	out, err := (Engine{}).Apply(
		ctx, Turn{ID: "t1"}, definition(),
		Start{
			ID:    "start",
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		Bindings{Client: client, Recorder: NewMemoryRecorder()},
	)
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("interruption=%+v err=%v", out.Stop, err)
	}
	var partial *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled {
			partial = step.Model
		}
	}
	if partial == nil || !partial.Interrupted || partial.Result == nil ||
		partial.Result.Message.Text() != "partial" ||
		partial.Result.Usage == nil || partial.Result.Usage.InputTokens != 5 {
		t.Fatalf("partial result=%+v", partial)
	}
	messages, err := Project(out.Turn)
	if err != nil || len(messages) != 1 || out.Turn.AnswerText() != "" {
		t.Fatalf("partial output reached model history: messages=%+v err=%v",
			messages, err)
	}
}

func TestContinueRecoversCrashedModelIntent(t *testing.T) {
	def := definition()
	rec := NewMemoryRecorder()
	turn := seedOpenModelIntent(t, rec, def, nil)
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Engine{}).Apply(
		context.Background(), turn, def, Continue{ID: "recover"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("recovery=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Interruptions != 1 || view.ModelAttempts != 1 {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	interrupted := 0
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled && step.Model.Interrupted {
			interrupted++
		}
	}
	if interrupted != 1 {
		t.Fatalf("recorded interrupted calls=%d", interrupted)
	}
}

func TestCrashedModelWithUnsafeHostedToolRequiresReport(t *testing.T) {
	def := definition()
	rec := NewMemoryRecorder()
	hosted := tool.Definition{
		Name: "remote",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "remote",
		},
	}
	turn := seedOpenModelIntent(t, rec, def, []tool.Definition{hosted})
	client := &scriptedClient{results: []llm.Result{final("must not send")}}
	out, err := (Engine{}).Apply(
		context.Background(), turn, def, Continue{ID: "recover"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || out.Stop.Kind != StopUncertain || client.sends != 0 ||
		len(out.Pending) != 1 {
		t.Fatalf("recovery=%+v err=%v sends=%d", out, err, client.sends)
	}
	answer := final("reconciled")
	accepted, err := (Engine{}).Apply(
		context.Background(), out.Turn, def,
		Accept{
			ID:          "report",
			CallID:      "t1:model:1",
			ModelResult: &answer,
		},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 0 {
		t.Fatalf("report=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	done, err := (Engine{}).Apply(
		context.Background(), accepted.Turn, def,
		Continue{ID: "finish"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 0 {
		t.Fatalf("finish=%+v err=%v sends=%d", done, err, client.sends)
	}
}

func TestTurnDeadlineLimitsRunningModelCall(t *testing.T) {
	for _, test := range []struct {
		name       string
		turnLimit  bool
		want       StopKind
		wantReason string
	}{
		{name: "turn deadline", turnLimit: true, want: StopLimited,
			wantReason: "deadline"},
		{name: "host deadline", want: StopInterrupted,
			wantReason: context.DeadlineExceeded.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			def := definition()
			ctx := context.Background()
			if test.turnLimit {
				def.Limits.Deadline = time.Now().Add(25 * time.Millisecond)
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
			}
			client := &interruptionClient{
				scriptedClient: &scriptedClient{},
				onSend: func(ctx context.Context, _ llm.Sink, _ int) (
					llm.Result, error,
				) {
					<-ctx.Done()
					return llm.Result{
						Usage: &llm.Usage{InputTokens: 4},
					}, ctx.Err()
				},
			}
			out, err := (Engine{}).Apply(
				ctx, Turn{ID: "t1"}, def,
				Start{
					ID: "start",
					Input: []llm.Message{
						llm.UserMessage(llm.Text("go")),
					},
				},
				Bindings{Client: client, Recorder: NewMemoryRecorder()},
			)
			if err != nil || out.Stop.Kind != test.want ||
				out.Stop.Reason != test.wantReason {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			view, err := Replay(out.Turn)
			if err != nil || view.ModelAttempts != 0 ||
				view.Spend.Usage.InputTokens != 4 {
				t.Fatalf("view=%+v err=%v", view, err)
			}
		})
	}
}

func TestPauseDuringModelCallStopsBeforeTool(t *testing.T) {
	pause := make(chan struct{})
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(_ context.Context, _ llm.Sink, index int) (
			llm.Result, error,
		) {
			if index == 0 {
				close(pause)
				return callResult("call", "work"), nil
			}
			return final("done"), nil
		},
	}
	runs := 0
	local := tool.Tool{
		Definition: tool.Definition{
			Name:   "work",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (
			tool.Outcome, error,
		) {
			runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("done")),
			}, nil
		}),
	}
	rec := NewMemoryRecorder()
	binding := Bindings{
		Client:   client,
		Tools:    fixedTools{local},
		Recorder: rec,
		Policy:   AllowLocalPolicy{},
		Pause:    pause,
	}
	first, err := (Engine{}).Apply(
		context.Background(), Turn{ID: "t1"}, definition(),
		Start{
			ID:    "start",
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		binding,
	)
	if err != nil || first.Stop.Kind != StopInterrupted ||
		first.Stop.Reason != "pause requested" || runs != 0 ||
		client.sends != 1 {
		t.Fatalf("pause=%+v err=%v runs=%d sends=%d",
			first.Stop, err, runs, client.sends)
	}
	binding.Pause = nil
	second, err := (Engine{}).Apply(
		context.Background(), first.Turn, definition(),
		Continue{ID: "resume"}, binding,
	)
	if err != nil || second.Stop.Kind != StopCompleted || runs != 1 ||
		client.sends != 2 {
		t.Fatalf("resume=%+v err=%v runs=%d sends=%d",
			second.Stop, err, runs, client.sends)
	}
}

func TestLeaseLossRejectsLateModelSettlement(t *testing.T) {
	rec := NewMemoryRecorder()
	rec.Claim("t1", "worker-a")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, _ llm.Sink, index int) (
			llm.Result, error,
		) {
			if index == 0 {
				rec.Claim("t1", "worker-b")
				cancel(errors.New("lease lost"))
				return llm.Result{}, ctx.Err()
			}
			return final("done"), nil
		},
	}
	first, err := (Engine{}).Apply(
		ctx, Turn{ID: "t1"}, definition(),
		Start{
			ID:    "start",
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		Bindings{Client: client, Recorder: rec, Owner: "worker-a"},
	)
	if err != nil || first.Persistence != PersistenceRejected ||
		!errors.Is(first.WriteError, ErrRevisionConflict) {
		t.Fatalf("stale write=%+v err=%v", first, err)
	}
	saved, err := rec.Load("t1")
	if err != nil || len(saved.Steps) != 2 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	resumed, err := (Engine{}).Apply(
		context.Background(), saved, definition(),
		Continue{ID: "new-worker"},
		Bindings{Client: client, Recorder: rec, Owner: "worker-b"},
	)
	if err != nil || resumed.Stop.Kind != StopCompleted ||
		client.sends != 2 {
		t.Fatalf("resumed=%+v err=%v sends=%d",
			resumed.Stop, err, client.sends)
	}
}

func seedOpenModelIntent(
	t *testing.T,
	rec *MemoryRecorder,
	def Definition,
	defs []tool.Definition,
) Turn {
	t.Helper()
	turn := Turn{ID: "t1"}
	for _, step := range []Step{
		{
			Kind: StepStarted,
			Started: &StartFact{
				Definition: def,
				Input:      []llm.Message{llm.UserMessage(llm.Text("go"))},
			},
		},
		{
			Kind: StepModelIntent,
			Model: &ModelFact{
				AttemptID:     "t1:model:1",
				MessageID:     "t1:model:1:message",
				RequestDigest: "request-digest",
				Provider:      def.Provider,
				API:           def.API,
				Model:         def.Model,
				Tools:         defs,
			},
		},
	} {
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(context.Background(), CommitRequest{
			Step: step, ExpectedSeq: step.Seq - 1,
		}); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	return turn
}
