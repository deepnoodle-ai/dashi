package dashi

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// attemptClient records the attempt identity each Send receives.
type attemptClient struct {
	*scriptedClient
	sent   []llm.Attempt
	onSend func(llm.Attempt)
}

func (c *attemptClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	if call.Attempt.IsZero() {
		return llm.Result{}, errors.New("send without attempt identity")
	}
	c.sent = append(c.sent, call.Attempt)
	if c.onSend != nil {
		c.onSend(call.Attempt)
	}
	return c.scriptedClient.Send(ctx, call)
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
	rec := NewMemoryRecorder()
	client := &attemptClient{
		scriptedClient: &scriptedClient{results: []llm.Result{final("done")}},
	}
	client.onSend = func(a llm.Attempt) {
		// The intent carrying this ID is durable before Send is called.
		saved, err := rec.Load(t.Context(), "t1")
		if err != nil {
			t.Fatal(err)
		}
		last := saved.Steps[len(saved.Steps)-1]
		if last.Kind != StepModelIntent || last.Model.AttemptID != a.AttemptID {
			t.Fatalf(
				"send %q before its intent was recorded: last step %s",
				a.AttemptID,
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
	intents := stepAttempts(out.Turn, StepModelIntent)
	if want := []string{"t1:model:1"}; !slices.Equal(intents, want) {
		t.Fatalf("intents=%v", intents)
	}
	if got := attemptIDs(t, client.sent); !slices.Equal(got, intents) {
		t.Fatalf("sent=%v intents=%v", got, intents)
	}
}

func TestInterruptedAttemptIsResentUnderNewID(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	client := &attemptClient{
		scriptedClient: &scriptedClient{results: []llm.Result{{}, final("done")}},
	}
	client.onSend = func(llm.Attempt) {
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
	intents := stepAttempts(out.Turn, StepModelIntent)
	if want := []string{
		"t1:model:1",
		"t1:model:2",
	}; !slices.Equal(intents, want) {
		t.Fatalf("intents=%v", intents)
	}
	if got := attemptIDs(t, client.sent); !slices.Equal(got, intents) {
		t.Fatalf("sent=%v intents=%v", got, intents)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelSettled &&
			step.Model.AttemptID == "t1:model:1" &&
			step.Model.Interrupted == InterruptNone {
			t.Fatalf("first attempt settled without interruption: %+v", step.Model)
		}
	}
}

func TestCrashedIntentIsNeverSentUnderItsID(t *testing.T) {
	def := testAgent()
	rec := NewMemoryRecorder()
	turn := seedOpenModelIntent(t, rec, def, nil)
	client := &attemptClient{
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
	if got, want := attemptIDs(t, client.sent), []string{
		"t1:model:2",
	}; !slices.Equal(got, want) {
		t.Fatalf("sent=%v, want %v", got, want)
	}
	settled := stepAttempts(out.Turn, StepModelSettled)
	if want := []string{
		"t1:model:1",
		"t1:model:2",
	}; !slices.Equal(settled, want) {
		t.Fatalf("settled=%v", settled)
	}
}
