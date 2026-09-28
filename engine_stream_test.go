package kernel

import (
	"context"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func streamStart() Start {
	return Start{
		ID:    "start",
		Input: []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}
}

func TestEngineStreamIsLazyOrderedAndTerminal(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{final("done")},
		deltas:  []llm.Delta{{Kind: llm.DeltaText, Text: "done"}},
	}
	recorder := NewMemoryRecorder()
	sequence := (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		streamStart(),
		Bindings{Client: client, Recorder: recorder},
	)
	if client.sends != 0 {
		t.Fatal("constructing iterator dispatched model")
	}
	var got []string
	var terminal *Outcome
	for event, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case event.Step != nil:
			got = append(got, string(event.Step.Kind))
		case event.Delta != nil:
			got = append(got, "delta:"+event.Delta.Text)
		case event.Outcome != nil:
			got = append(got, "outcome")
			terminal = event.Outcome
		default:
			t.Fatal("empty stream event")
		}
	}
	want := []string{
		"turn_started",
		"model_requested",
		"delta:done",
		"model_settled",
		"turn_stopped",
		"outcome",
	}
	if len(got) != len(want) {
		t.Fatalf("events=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events=%v", got)
		}
	}
	if terminal == nil || terminal.Stop.Kind != StopCompleted || client.sends != 1 || len(client.inputs) != 1 || !client.inputs[0].Stream {
		t.Fatalf(
			"outcome=%+v sends=%d inputs=%+v",
			terminal,
			client.sends,
			client.inputs,
		)
	}
	for event, err := range sequence {
		if !errors.Is(
			err,
			ErrStreamConsumed,
		) || event.Step != nil || event.Outcome != nil {
			t.Fatalf("second range event=%+v err=%v", event, err)
		}
	}
	if client.sends != 1 {
		t.Fatal("second range repeated turn work")
	}
}

func TestEngineStreamValidationAndRejectedWrite(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	for event, err := range (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		Start{Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}},
		Bindings{Client: client, Recorder: NewMemoryRecorder()},
	) {
		if err == nil || event.Step != nil || event.Outcome != nil {
			t.Fatalf("validation event=%+v err=%v", event, err)
		}
	}
	if client.sends != 0 {
		t.Fatal("invalid command dispatched model")
	}
	recorder := &failOnceKind{base: NewMemoryRecorder(), kind: StepStarted}
	steps, terminals := 0, 0
	for event, err := range (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		streamStart(),
		Bindings{Client: client, Recorder: recorder},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Step != nil {
			steps++
		}
		if event.Outcome != nil {
			terminals++
			if event.Outcome.Persistence != PersistenceRejected {
				t.Fatalf("outcome=%+v", event.Outcome)
			}
		}
	}
	if steps != 0 || terminals != 1 || client.sends != 0 {
		t.Fatalf("steps=%d terminals=%d sends=%d", steps, terminals, client.sends)
	}
}

func TestEngineStreamBreakBeforeDispatchRecordsInterruption(t *testing.T) {
	for _, stopAt := range []StepKind{StepStarted, StepModelIntent} {
		t.Run(string(stopAt), func(t *testing.T) {
			client := &scriptedClient{results: []llm.Result{final("unused")}}
			recorder := NewMemoryRecorder()
			for event, err := range (Engine{}).Stream(
				context.Background(),
				Turn{ID: "t1"},
				definition(),
				streamStart(),
				Bindings{Client: client, Recorder: recorder},
			) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Step != nil && event.Step.Kind == stopAt {
					break
				}
			}
			turn, err := recorder.Load(context.Background(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			view, err := Replay(turn)
			if err != nil || view.Stop.Kind != StopInterrupted || client.sends != 0 {
				t.Fatalf("view=%+v err=%v sends=%d", view, err, client.sends)
			}
			if stopAt == StepModelIntent {
				found := false
				for _, step := range turn.Steps {
					if step.Kind == StepModelSettled && step.Model != nil &&
						step.Model.Interrupted {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing interrupted settlement: %+v", turn.Steps)
				}
			}
		})
	}
}

type cancelAfterDeltaClient struct{ *scriptedClient }

func (c *cancelAfterDeltaClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	c.sends++
	call.Sink(llm.Delta{Kind: llm.DeltaText, Text: "partial"})
	<-ctx.Done()
	return llm.Result{
		ID:        "partial",
		Message:   llm.AssistantMessage(llm.Text("partial")),
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
		Usage:     &llm.Usage{InputTokens: 3, OutputTokens: 2},
	}, ctx.Err()
}

func TestStreamBreakKeepsInterruptedPartialResult(t *testing.T) {
	client := &cancelAfterDeltaClient{scriptedClient: &scriptedClient{}}
	recorder := NewMemoryRecorder()
	for event, err := range (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		streamStart(),
		Bindings{Client: client, Recorder: recorder},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil {
			break
		}
	}
	turn, err := recorder.Load(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(turn)
	if err != nil || view.Stop.Kind != StopInterrupted || client.sends != 1 {
		t.Fatalf("view=%+v err=%v sends=%d", view, err, client.sends)
	}
	if view.ModelAttempts != 0 || view.Spend.Usage.InputTokens != 3 ||
		view.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("attempts=%d usage=%+v", view.ModelAttempts, view.Spend.Usage)
	}
	found := false
	for _, step := range turn.Steps {
		if step.Kind == StepModelSettled && step.Model != nil &&
			step.Model.Interrupted && step.Model.Result != nil &&
			step.Model.Result.ID == "partial" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing interrupted partial settlement: %+v", turn.Steps)
	}
}

func TestEngineStreamBreakAfterToolStartLeavesCallPending(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call", "work")}}
	recorder := NewMemoryRecorder()
	runs := 0
	local := tool.Tool{
		Definition: tool.Definition{
			Name:   "work",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{State: tool.StateSucceeded}, nil
		}),
	}
	for event, err := range (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		streamStart(),
		Bindings{
			Client:   client,
			Tools:    fixedTools{local},
			Recorder: recorder,
			Policy:   AllowLocalPolicy{},
		},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Step != nil && event.Step.Kind == StepToolStarted {
			break
		}
	}
	turn, err := recorder.Load(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(turn)
	if err != nil || view.Stop.Kind != StopInterrupted || runs != 0 ||
		view.Tools["call"].Status != ToolStarted {
		t.Fatalf("view=%+v err=%v runs=%d", view, err, runs)
	}
}

func TestEngineStreamBreakWithContextCancelEndsTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call", "work")}}
	recorder := NewMemoryRecorder()
	for event, err := range (Engine{}).Stream(
		context.Background(),
		Turn{ID: "t1"},
		definition(),
		streamStart(),
		Bindings{
			Client:        client,
			Recorder:      recorder,
			OnContextDone: ContextCancel,
		},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Step != nil && event.Step.Kind == StepModelSettled {
			break
		}
	}
	turn, err := recorder.Load(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind != StopCanceled {
		t.Fatalf("stop=%+v, want canceled", view.Stop)
	}
	if view.Tools["call"].Status != ToolSettled {
		t.Fatalf("unstarted call status=%s, want settled", view.Tools["call"].Status)
	}
	if view.Tools["call"].Outcome == nil ||
		view.Tools["call"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf(
			"unstarted call outcome=%+v, want not executed",
			view.Tools["call"].Outcome,
		)
	}
}
