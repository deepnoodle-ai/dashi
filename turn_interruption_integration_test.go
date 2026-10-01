package dashi

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type interruptionClient struct {
	*scriptedClient
	onSend func(context.Context, llm.Sink, int) (llm.Result, error)
}

func (c *interruptionClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	index := c.sends
	c.sends++
	return c.onSend(ctx, call.Sink, index)
}

func TestRepeatedModelInterruptionsKeepUsageAndModelCallAllowance(
	t *testing.T,
) {
	rec := NewMemoryRecorder()
	def := testAgent()
	def.Limits.Resumes = 2
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
	for i := range 3 {
		ctx, stop := context.WithCancelCause(t.Context())
		cancel = stop
		var command Command = Continue{CommandID: "continue-" + string(rune('a'+i))}
		if i == 0 {
			command = Start{
				CommandID: "start",
				Agent:     def,
				Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
			}
		}
		out, err := (Runner{Client: client, Recorder: rec}).Run(
			ctx,
			turn,
			command,
		)
		if err != nil || out.Stop.Kind != StopInterrupted ||
			out.Stop.Reason != "deploying" {
			t.Fatalf("interruption %d: stop=%+v err=%v", i, out.Stop, err)
		}
		turn = out.Turn
		view, err := turn.View()
		if err != nil {
			t.Fatal(err)
		}
		if view.ModelCalls != 0 || view.Resumes != i ||
			view.Spend.Usage.InputTokens != 2*(i+1) ||
			view.Spend.Usage.OutputTokens != 2*(i+1) {
			t.Fatalf("interruption %d: view=%+v", i, view)
		}
		if out.ReadyToContinue != (i < def.Limits.Resumes) ||
			view.ReadyToContinue != out.ReadyToContinue ||
			answerText(turn.Steps) != "" {
			t.Fatalf("interruption %d: outcome=%+v view=%+v", i, out, view)
		}
	}
	limited, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "over-limit"},
	)
	if err != nil || limited.Stop.Kind != StopLimited ||
		limited.Stop.Reason != "resumes" || client.sends != 3 {
		t.Fatalf("limit=%+v err=%v sends=%d", limited.Stop, err, client.sends)
	}
	view, err := limited.Turn.View()
	if err != nil || view.ModelCalls != 0 || view.Resumes != 2 ||
		view.Spend.Usage.InputTokens != 6 ||
		view.ReadyToContinue || limited.ReadyToContinue {
		t.Fatalf("limited view=%+v err=%v", view, err)
	}
	last := limited.Turn.Steps[len(limited.Turn.Steps)-1]
	if last.Kind != StepTurnStopped || last.CommandID != "over-limit" {
		t.Fatalf("excess continue ran work: %+v", last)
	}
	for _, step := range limited.Turn.Steps {
		if step.Kind == StepModelCallFinished &&
			step.Model.Interrupted == InterruptNone {
			t.Fatalf("model call result was actionable: %+v", step.Model)
		}
	}
}

func TestInterruptedModelKeepsDeltaOnlyPartialOutputAndUsage(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, sink llm.Sink, _ int) (
			llm.Result, error,
		) {
			sink(llm.Delta{Kind: llm.DeltaText, Text: "part"})
			sink(llm.Delta{Kind: llm.DeltaText, Text: "ial"})
			sink(llm.Delta{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 5}})
			cancel(errors.New("worker stopped"))
			return llm.Result{}, ctx.Err()
		},
	}
	out, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("interruption=%+v err=%v", out.Stop, err)
	}
	var partial *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished {
			partial = step.Model
		}
	}
	if partial == nil || partial.Interrupted != InterruptRun ||
		partial.Result == nil ||
		partial.Result.Message.Text() != "partial" ||
		partial.Result.Usage == nil || partial.Result.Usage.InputTokens != 5 {
		t.Fatalf("partial result=%+v", partial)
	}
	messages, err := TurnMessagesFull(out.Turn)
	if err != nil || len(messages) != 1 || out.Reply != "" {
		t.Fatalf(
			"partial output reached model history: messages=%+v err=%v",
			messages,
			err,
		)
	}
}

func TestContinueRecoversCrashedModelCallStart(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	turn := seedOpenModelCallStart(t, rec, def, nil)
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "recover"},
	)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("recovery=%+v err=%v sends=%d", out.Stop, err, client.sends)
	}
	view, err := out.Turn.View()
	if err != nil || view.Resumes != 1 || view.ModelCalls != 1 {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	var causes []InterruptCause
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallFinished &&
			step.Model.Interrupted != InterruptNone {
			causes = append(causes, step.Model.Interrupted)
		}
	}
	if !slices.Equal(causes, []InterruptCause{InterruptWorker}) {
		t.Fatalf("recovered call causes=%v, want one worker", causes)
	}
}

func TestCrashedModelWithUnsafeProviderToolRequiresReport(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	providerTool := tool.Definition{
		Name: "remote",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "remote",
		},
	}
	turn := seedOpenModelCallStart(t, rec, def, []tool.Definition{providerTool})
	client := &scriptedClient{results: []llm.Result{final("must not send")}}
	out, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "recover"},
	)
	if err != nil || out.Stop.Kind != StopResultUnknown || client.sends != 0 ||
		len(out.Pending) != 1 {
		t.Fatalf("recovery=%+v err=%v sends=%d", out, err, client.sends)
	}
	answer := final("resolved")
	accepted, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		out.Turn,
		Resolve{
			CommandID:   "report",
			CallID:      "t1:model:1",
			ModelResult: &answer,
		},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 0 {
		t.Fatalf("report=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	done, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		accepted.Turn,
		Continue{CommandID: "finish"},
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
		{
			name:       "turn deadline",
			turnLimit:  true,
			want:       StopLimited,
			wantReason: "deadline",
		},
		{
			name:       "host deadline",
			want:       StopInterrupted,
			wantReason: context.DeadlineExceeded.Error(),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			def := testAgent()
			ctx := t.Context()
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
					return llm.Result{Usage: &llm.Usage{InputTokens: 4}}, ctx.Err()
				},
			}
			out, err := (Runner{
				Client:   client,
				Recorder: NewMemoryRecorder(),
			}).Run(
				ctx,
				Turn{ID: "t1"},
				Start{
					CommandID: "start",
					Agent:     def,
					Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
				},
			)
			if err != nil || out.Stop.Kind != test.want ||
				out.Stop.Reason != test.wantReason {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			view, err := out.Turn.View()
			if err != nil || view.ModelCalls != 0 ||
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
				Output: new(tool.TextOutput("done")),
			}, nil
		}),
	}
	rec := NewMemoryRecorder()
	runner := Runner{
		Client:   client,
		Tools:    tool.List{local},
		Recorder: rec,
	}
	first, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted ||
		first.Stop.Reason != "pause requested" || runs != 0 ||
		client.sends != 1 {
		t.Fatalf(
			"pause=%+v err=%v runs=%d sends=%d",
			first.Stop,
			err,
			runs,
			client.sends,
		)
	}
	second, err := runner.Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "resume"},
	)
	if err != nil || second.Stop.Kind != StopCompleted || runs != 1 ||
		client.sends != 2 {
		t.Fatalf(
			"resume=%+v err=%v runs=%d sends=%d",
			second.Stop,
			err,
			runs,
			client.sends,
		)
	}
}

func TestLeaseLossRejectsLateModelFinish(t *testing.T) {
	rec := NewMemoryRecorder()
	rec.Claim(t.Context(), "t1", "worker-a")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &interruptionClient{
		scriptedClient: &scriptedClient{},
		onSend: func(ctx context.Context, _ llm.Sink, index int) (
			llm.Result, error,
		) {
			if index == 0 {
				rec.Claim(t.Context(), "t1", "worker-b")
				cancel(errors.New("lease lost"))
				return llm.Result{}, ctx.Err()
			}
			return final("done"), nil
		},
	}
	first, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		WithOwner("worker-a"),
	)
	if !nothingWritten(err) ||
		!errors.Is(err, ErrStaleTurn) {
		t.Fatalf("stale write=%+v err=%v", first, err)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(saved.Steps) != 2 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	resumed, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		saved,
		Continue{CommandID: "new-worker"},
		WithOwner("worker-b"),
	)
	if err != nil || resumed.Stop.Kind != StopCompleted ||
		client.sends != 2 {
		t.Fatalf("resumed=%+v err=%v sends=%d", resumed.Stop, err, client.sends)
	}
}

func seedOpenModelCallStart(
	t *testing.T,
	rec *MemoryRecorder,
	def Agent,
	defs []tool.Definition,
) Turn {
	t.Helper()
	turn := Turn{ID: "t1"}
	for _, step := range []Step{
		{
			Kind: StepTurnStarted,
			Started: &StartFact{
				Agent: def,
				Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
			},
		},
		{
			Kind: StepModelCallStarted,
			Model: &ModelFact{
				ModelCallID: "t1:model:1",
				MessageID:   "t1:model:1:message",
				RequestHash: "request-hash",
				Provider:    def.Provider,
				API:         def.API,
				Model:       def.Model,
				Tools:       defs,
			},
		},
	} {
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(
			t.Context(),
			CommitRequest{Step: step, ExpectedSeq: step.Seq - 1},
		); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	return turn
}
