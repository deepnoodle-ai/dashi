package kernel

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

// attemptClient records the attempt identity each Prepare and Send receives.
type attemptClient struct {
	*scriptedClient
	prepared []llm.Attempt
	sent     []llm.Attempt
	onSend   func()
}

func (c *attemptClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	a, ok := llm.AttemptFrom(ctx)
	if !ok {
		return llm.Request{}, errors.New("prepare without attempt identity")
	}
	c.prepared = append(c.prepared, a)
	return c.scriptedClient.Prepare(ctx, in)
}

func (c *attemptClient) Send(ctx context.Context, req llm.Request, sink llm.Sink) (llm.Result, error) {
	a, ok := llm.AttemptFrom(ctx)
	if !ok {
		return llm.Result{}, errors.New("send without attempt identity")
	}
	c.sent = append(c.sent, a)
	if c.onSend != nil {
		c.onSend()
	}
	return c.scriptedClient.Send(ctx, req, sink)
}

func stepAttempts(turn Turn, kind StepKind) []string {
	var ids []string
	for _, step := range turn.Steps {
		if step.Kind == kind {
			ids = append(ids, step.Model.AttemptID)
		}
	}
	return ids
}

func attemptIDs(t *testing.T, attempts []llm.Attempt) []string {
	t.Helper()
	ids := make([]string, len(attempts))
	for i, a := range attempts {
		if a.TurnID != "t1" {
			t.Fatalf("attempt %d turn=%q, want t1", i, a.TurnID)
		}
		ids[i] = a.AttemptID
	}
	return ids
}

func TestClientReceivesRecordedAttemptIdentity(t *testing.T) {
	client := &attemptClient{scriptedClient: &scriptedClient{results: []llm.Result{final("done")}}}
	out, err := (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(),
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
		Bindings{Client: client, Recorder: NewMemoryRecorder()})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	intents := stepAttempts(out.Turn, StepModelIntent)
	if want := []string{"t1:model:1"}; !slices.Equal(intents, want) {
		t.Fatalf("intents=%v", intents)
	}
	if got := attemptIDs(t, client.prepared); !slices.Equal(got, intents) {
		t.Fatalf("prepared=%v intents=%v", got, intents)
	}
	if got := attemptIDs(t, client.sent); !slices.Equal(got, intents) {
		t.Fatalf("sent=%v intents=%v", got, intents)
	}
}

func TestInterruptedAttemptIsResentUnderNewID(t *testing.T) {
	def := definition()
	rec := NewMemoryRecorder()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	client := &attemptClient{scriptedClient: &scriptedClient{
		results: []llm.Result{{}, final("done")},
	}}
	client.onSend = func() {
		if len(client.sent) == 1 {
			cancel(errors.New("deploying"))
		}
	}
	first, err := (Engine{}).Apply(ctx, Turn{ID: "t1"}, def,
		Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
		Bindings{Client: client, Recorder: rec})
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("first stop=%+v err=%v", first.Stop, err)
	}
	out, err := (Engine{}).Apply(context.Background(), first.Turn, def,
		Continue{ID: "continue"}, Bindings{Client: client, Recorder: rec})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("continue stop=%+v err=%v", out.Stop, err)
	}
	intents := stepAttempts(out.Turn, StepModelIntent)
	if want := []string{"t1:model:1", "t1:model:2"}; !slices.Equal(intents, want) {
		t.Fatalf("intents=%v", intents)
	}
	if got := attemptIDs(t, client.sent); !slices.Equal(got, intents) {
		t.Fatalf("sent=%v intents=%v", got, intents)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled && step.Model.AttemptID == "t1:model:1" && !step.Model.Interrupted {
			t.Fatalf("first attempt settled without interruption: %+v", step.Model)
		}
	}
}

func TestCrashedIntentIsNeverSentUnderItsID(t *testing.T) {
	def := definition()
	rec := NewMemoryRecorder()
	turn := seedOpenModelIntent(t, rec, def, nil)
	client := &attemptClient{scriptedClient: &scriptedClient{results: []llm.Result{final("done")}}}
	out, err := (Engine{}).Apply(context.Background(), turn, def,
		Continue{ID: "recover"}, Bindings{Client: client, Recorder: rec})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got, want := attemptIDs(t, client.sent), []string{"t1:model:2"}; !slices.Equal(got, want) {
		t.Fatalf("sent=%v, want %v", got, want)
	}
	settled := stepAttempts(out.Turn, StepModelSettled)
	if want := []string{"t1:model:1", "t1:model:2"}; !slices.Equal(settled, want) {
		t.Fatalf("settled=%v", settled)
	}
}
