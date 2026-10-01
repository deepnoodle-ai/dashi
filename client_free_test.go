package dashi

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// noModelClient fails the test if a run prepares or sends a model request.
type noModelClient struct{ t *testing.T }

func (noModelClient) Provider() string { return "test" }

func (noModelClient) API() string { return "test.chat" }

func (noModelClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c noModelClient) Prepare(context.Context, llm.Input) (llm.Request, error) {
	c.t.Helper()
	c.t.Fatal("unexpected Prepare")
	return llm.Request{}, nil
}

func (c noModelClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.t.Helper()
	c.t.Fatal("unexpected Send")
	return llm.Result{}, nil
}

func TestStartAndContinueRequireClient(t *testing.T) {
	rec := NewMemoryRecorder()
	runner := Runner{Recorder: rec}
	_, err := runner.Run(t.Context(), Turn{}, Start{
		Agent: testAgent(),
		Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	})
	if err == nil || !strings.Contains(err.Error(), "client is required") {
		t.Fatalf("start err=%v", err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, rec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(t.Context(), out.Turn, Continue{})
	if err == nil || !strings.Contains(err.Error(), "client is required") {
		t.Fatalf("continue err=%v", err)
	}
}

func TestCancelWithoutClient(t *testing.T) {
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	waiting, err := start(t, client, tool.List{external}, rec, Hooks{})
	if err != nil || waiting.Stop.Kind != StopWaiting {
		t.Fatalf("waiting=%+v err=%v", waiting.Stop, err)
	}
	out, err := (Runner{Recorder: rec}).Run(
		t.Context(),
		waiting.Turn,
		Cancel{CommandID: "cancel", Reason: "operator"},
	)
	if err != nil || out.Stop.Kind != StopCanceled || out.Next() != NextNone {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
}

func TestCancelRefusesTurnThatHasNotStarted(t *testing.T) {
	_, err := (Runner{Recorder: NewMemoryRecorder()}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Cancel{CommandID: "cancel"},
	)
	if err == nil || !strings.Contains(err.Error(), "turn has not started") {
		t.Fatalf("err=%v", err)
	}
}

func TestSettleAndDeliverWithoutClient(t *testing.T) {
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	clientless := func(rec Recorder) Runner {
		return Runner{Recorder: rec, Tools: tool.List{external}}
	}

	t.Run("tool outcome and deliver", func(t *testing.T) {
		rec := NewMemoryRecorder()
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		waiting, err := start(t, client, tool.List{external}, rec, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		delivered, err := clientless(rec).Run(t.Context(), waiting.Turn, Deliver{
			CommandID: "note",
			Items: []ContextItem{{
				ID:      "note-1",
				Message: llm.UserMessage(llm.Text("also check the cache")),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := tool.Succeeded("found")
		settled, err := clientless(rec).Run(
			t.Context(),
			delivered.Turn,
			Settle{CommandID: "result", CallID: "call-1", Outcome: &result},
		)
		if err != nil || !settled.ReadyToContinue || client.sends != 1 {
			t.Fatalf("settled=%+v err=%v sends=%d", settled.Stop, err, client.sends)
		}
	})

	t.Run("approval", func(t *testing.T) {
		rec := NewMemoryRecorder()
		local := tool.Tool{
			Definition: tool.Definition{
				Name:   "lookup",
				Schema: []byte(`{"type":"object"}`),
			},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				return tool.Succeeded("ok"), nil
			}),
		}
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		waiting, err := start(t, client, tool.List{local}, rec, RequireApproval())
		if err != nil || waiting.Next() != NextAnswer {
			t.Fatalf("waiting=%+v err=%v", waiting.Stop, err)
		}
		approved, err := (Runner{Recorder: rec}).Run(
			t.Context(),
			waiting.Turn,
			waiting.Pending[0].Deny("not now"),
		)
		if err != nil || client.sends != 1 {
			t.Fatalf("approved=%+v err=%v", approved.Stop, err)
		}
	})

	for _, tc := range []struct {
		name   string
		settle Settle
	}{
		{"model result", Settle{ModelResult: new(final("recovered"))}},
		{"model failure", Settle{ModelFailure: &ModelFailure{
			Disposition: llm.FailureNotDispatched,
			Reason:      "worker did not send",
			Evidence:    "dispatch log",
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewMemoryRecorder()
			turn := crashedModelIntent(t, rec)
			tc.settle.CommandID = "reconcile"
			tc.settle.CallID = "t1:model:1"
			out, err := (Runner{Recorder: rec}).Run(t.Context(), turn, tc.settle)
			if err != nil || !out.ReadyToContinue {
				t.Fatalf("out=%+v err=%v", out.Stop, err)
			}
		})
	}
}

// crashedModelIntent records a turn whose model attempt has an intent and
// an uncertain stop, so a Settle must reconcile it.
func crashedModelIntent(t *testing.T, rec *MemoryRecorder) Turn {
	t.Helper()
	def := testAgent()
	turn := Turn{ID: "t1"}
	for _, step := range []Step{
		{Kind: StepStarted, Started: &StartFact{
			Agent: def,
			Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		}},
		{Kind: StepModelIntent, Model: &ModelFact{
			AttemptID:     "t1:model:1",
			MessageID:     "t1:model:1:message",
			RequestDigest: "digest",
			Provider:      def.Provider,
			API:           def.API,
			Model:         def.Model,
		}},
		{Kind: StepStopped, Stop: &Stop{Kind: StopUncertain, Reason: "worker crashed"}},
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

func TestAdmitWithoutRunning(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	admitted, err := (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	steps := admitted.Turn.Steps
	if len(steps) != 2 ||
		steps[0].Kind != StepStarted ||
		steps[1].Kind != StepStopped ||
		steps[1].Stop.Kind != StopInterrupted ||
		steps[1].Stop.Reason != "pause requested" {
		t.Fatalf("steps=%+v", steps)
	}
	if admitted.Next() != NextContinue || !admitted.ReadyToContinue ||
		admitted.ModelAttempts != 0 || admitted.Interruptions != 0 {
		t.Fatalf("view=%+v next=%v", admitted.TurnView, admitted.Next())
	}

	canceled, err := (Runner{Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Cancel{CommandID: "cancel", Reason: "admission withdrawn"},
	)
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("canceled=%+v err=%v", canceled.Stop, err)
	}

	// The same admission, continued later, runs normally.
	rec = NewMemoryRecorder()
	admitted, err = (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t2"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Continue{CommandID: "run"},
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("done=%+v err=%v sends=%d", done.Stop, err, client.sends)
	}
	// A pause before the first model attempt interrupted no work.
	if done.Interruptions != 0 {
		t.Fatalf("interruptions=%d", done.Interruptions)
	}
}

func TestAdmittedTurnResumesWithNoInterruptionsAllowed(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	agent := testAgent()
	agent.Limits.Interruptions = -1
	admitted, err := (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil || admitted.Next() != NextContinue {
		t.Fatalf("admitted=%+v err=%v", admitted.Stop, err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Continue{CommandID: "run"},
	)
	if err != nil || done.Stop.Kind != StopCompleted || done.Interruptions != 0 {
		t.Fatalf("done=%+v interruptions=%d err=%v", done.Stop, done.Interruptions, err)
	}
}

// pausingTool is a local "lookup" tool that closes paused when it runs, so
// a turn pauses after a model attempt, before its next model call.
func pausingTool(paused chan struct{}) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			close(paused)
			return tool.Succeeded("ok"), nil
		}),
	}
}

func TestPauseAfterModelAttemptCountsAsInterruption(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	local := pausingTool(paused)
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "lookup"),
		final("done"),
	}}
	runner := Runner{Client: client, Tools: tool.List{local}, Recorder: rec}
	stopped, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil || stopped.Stop.Kind != StopInterrupted || client.sends != 1 {
		t.Fatalf("stopped=%+v err=%v sends=%d", stopped.Stop, err, client.sends)
	}
	done, err := runner.Run(t.Context(), stopped.Turn, Continue{CommandID: "run"})
	if err != nil || done.Stop.Kind != StopCompleted || done.Interruptions != 1 {
		t.Fatalf("done=%+v interruptions=%d err=%v", done.Stop, done.Interruptions, err)
	}
}
