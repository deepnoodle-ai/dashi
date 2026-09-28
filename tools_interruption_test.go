package kernel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestContinueRepeatsOnlySafeStartedLocalTools(t *testing.T) {
	tests := []struct {
		name        string
		annotations tool.Annotations
		wantRuns    int
		wantStop    StopKind
	}{
		{
			name: "read only",
			annotations: tool.Annotations{
				ReadOnly: ptr(true),
			},
			wantRuns: 1,
			wantStop: StopCompleted,
		},
		{
			name: "idempotent",
			annotations: tool.Annotations{
				Idempotent: ptr(true),
			},
			wantRuns: 1,
			wantStop: StopCompleted,
		},
		{
			name:     "unannotated",
			wantStop: StopUncertain,
		},
		{
			name: "explicitly not safe",
			annotations: tool.Annotations{
				ReadOnly:   ptr(false),
				Idempotent: ptr(false),
			},
			wantStop: StopUncertain,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toolDef := tool.Definition{
				Name:        "work",
				Schema:      []byte(`{"type":"object"}`),
				Kind:        tool.KindLocal,
				Annotations: tc.annotations,
			}
			rec := NewMemoryRecorder()
			turn := startedLocalToolTurn(t, rec, toolDef)
			client := &scriptedClient{
				results: []llm.Result{final("done")},
			}
			runs := 0
			bound := tool.Tool{
				Definition: toolDef,
				Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
					runs++
					return tool.Outcome{
						State:  tool.StateSucceeded,
						Output: ptr(tool.TextOutput("done")),
					}, nil
				}),
			}
			out, err := (Engine{}).Apply(
				context.Background(), turn, definition(),
				Continue{ID: "resume"}, Bindings{
					Client:   client,
					Tools:    fixedTools{bound},
					Recorder: rec,
					Policy:   AllowLocalPolicy{},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stop.Kind != tc.wantStop {
				t.Fatalf("stop=%+v; want %s", out.Stop, tc.wantStop)
			}
			if runs != tc.wantRuns {
				t.Fatalf("tool ran %d times; want %d", runs, tc.wantRuns)
			}
			starts := 0
			for _, step := range out.Turn.Steps {
				if step.Kind == StepToolStarted {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("record contains %d tool starts; want one", starts)
			}
		})
	}
}

func TestPauseDuringToolLoopSettlesRunningCallBeforeStopping(t *testing.T) {
	firstCall := tool.Call{ID: "first", Name: "first", Args: []byte(`{}`)}
	secondCall := tool.Call{ID: "second", Name: "second", Args: []byte(`{}`)}
	client := &scriptedClient{results: []llm.Result{
		{
			Message: llm.AssistantMessage(
				llm.Content{Kind: llm.ContentToolCall, ToolCall: &firstCall},
				llm.Content{Kind: llm.ContentToolCall, ToolCall: &secondCall},
			),
			Stop: llm.Stop{Kind: llm.StopToolUse},
		},
		final("done"),
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	pause := make(chan struct{})
	first := tool.Tool{
		Definition: tool.Definition{
			Name:   "first",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			close(started)
			<-release
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("first result")),
			}, nil
		}),
	}
	secondRuns := 0
	second := tool.Tool{
		Definition: tool.Definition{
			Name:   "second",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			secondRuns++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("second result")),
			}, nil
		}),
	}
	def := definition()
	def.Limits.ToolConcurrency = 1
	rec := NewMemoryRecorder()
	type result struct {
		out Outcome
		err error
	}
	finished := make(chan result, 1)
	go func() {
		out, err := (Engine{}).Apply(
			context.Background(), Turn{ID: "t1"}, def,
			Start{
				ID:    "start",
				Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
			},
			Bindings{
				Client:   client,
				Tools:    fixedTools{first, second},
				Recorder: rec,
				Policy:   AllowLocalPolicy{},
				Pause:    pause,
			},
		)
		finished <- result{out: out, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first tool did not start")
	}
	close(pause)
	close(release)
	var got result
	select {
	case got = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not pause")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.out.Stop.Kind != StopInterrupted ||
		got.out.Stop.Reason != "pause requested" {
		t.Fatalf("stop=%+v", got.out.Stop)
	}
	if secondRuns != 0 {
		t.Fatalf("second tool ran %d times before resume", secondRuns)
	}
	view, err := Replay(got.out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["first"].Status != ToolSettled {
		t.Fatalf("first tool status=%s; want settled", view.Tools["first"].Status)
	}
	secondStatus := view.Tools["second"].Status
	if secondStatus != ToolRequested && secondStatus != ToolAllowed {
		t.Fatalf("second tool status=%s; want pending", secondStatus)
	}
	resumed, err := (Engine{}).Apply(
		context.Background(), got.out.Turn, def,
		Continue{ID: "resume"}, Bindings{
			Client:   client,
			Tools:    fixedTools{first, second},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind != StopCompleted || secondRuns != 1 {
		t.Fatalf("resume stop=%+v; second runs=%d", resumed.Stop, secondRuns)
	}
}

func TestToolResultAfterContextEndIsDropped(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "work")},
	}
	rec := NewMemoryRecorder()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	returned := make(chan struct{})
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "work",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			cancel(errors.New("lease lost"))
			defer close(returned)
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: ptr(tool.TextOutput("late result")),
			}, nil
		}),
	}
	out, err := (Engine{}).Apply(
		ctx, Turn{ID: "t1"}, definition(),
		Start{
			ID:    "start",
			Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
		Bindings{
			Client:   client,
			Tools:    fixedTools{binding},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-returned
	if out.Stop.Kind != StopInterrupted || out.Stop.Reason != "lease lost" {
		t.Fatalf("stop=%+v", out.Stop)
	}
	saved, err := rec.Load(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(saved)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolStarted {
		t.Fatalf("tool status=%s; want started", view.Tools["call-1"].Status)
	}
	for _, step := range saved.Steps {
		if step.Kind == StepToolSettled && step.Tool.Call.ID == "call-1" {
			t.Fatal("late tool result was committed")
		}
	}
}

func startedLocalToolTurn(
	t *testing.T,
	rec *MemoryRecorder,
	toolDef tool.Definition,
) Turn {
	t.Helper()
	def := definition()
	turn := Turn{ID: "t1"}
	commit := func(step Step) {
		t.Helper()
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(
			context.Background(),
			CommitRequest{Step: step, ExpectedSeq: step.Seq - 1},
		); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	call := tool.Call{ID: "call-1", Name: "work", Args: []byte(`{}`)}
	commit(Step{
		Kind: StepStarted,
		Started: &StartFact{
			Definition: def,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("go")),
			},
		},
	})
	commit(Step{
		Kind: StepModelIntent,
		Model: &ModelFact{
			AttemptID:     "t1:model:1",
			MessageID:     "t1:model:1:message",
			RequestDigest: "digest",
			Provider:      def.Provider,
			API:           def.API,
			Model:         def.Model,
			Tools:         []tool.Definition{toolDef},
		},
	})
	requested := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: def.Provider, Model: def.Model},
	}
	commit(Step{
		Kind: StepModelSettled,
		Model: &ModelFact{
			AttemptID: "t1:model:1",
			MessageID: "t1:model:1:message",
			Result:    &requested,
		},
	})
	commit(Step{
		Kind: StepToolDecision,
		Tool: &ToolFact{
			Call:       call,
			Definition: toolDef,
			Decision: ToolDecision{
				Kind:     DecisionAllow,
				Revision: "r1",
			},
		},
	})
	commit(Step{
		Kind: StepToolStarted,
		Tool: &ToolFact{
			Call:       call,
			Definition: toolDef,
		},
	})
	return turn
}
