package llm

import (
	"context"
	"testing"
)

func TestAttemptIsZero(t *testing.T) {
	if !(Attempt{}).IsZero() {
		t.Fatal("empty attempt is not zero")
	}
	if (Attempt{TurnID: "t1", AttemptID: "t1:model:1"}).IsZero() {
		t.Fatal("set attempt is zero")
	}
}

func TestStreamPassesCallThrough(t *testing.T) {
	client := &iteratorClient{
		send: func(_ context.Context, sink Sink) (Result, error) {
			sink(Delta{Kind: DeltaText, Text: "hi"})
			return Result{Stop: Stop{Kind: StopFinished}}, nil
		},
	}
	want := Attempt{TurnID: "t1", AttemptID: "t1:model:1"}
	var observed []string
	for range Stream(
		context.Background(),
		client,
		Call{
			Attempt: want,
			Sink: func(d Delta) {
				observed = append(
					observed,
					d.Text,
				)
			},
		},
	) {
	}
	if client.last.Attempt != want {
		t.Fatalf("attempt=%+v, want %+v", client.last.Attempt, want)
	}
	if len(observed) != 1 || observed[0] != "hi" {
		t.Fatalf("caller sink saw %v", observed)
	}
}

func TestDirectCallsHaveNoAttempt(t *testing.T) {
	client := &iteratorClient{
		send: func(context.Context, Sink) (Result, error) {
			return Result{Stop: Stop{Kind: StopFinished}}, nil
		},
	}
	for range Stream(context.Background(), client, Call{}) {
	}
	if !client.last.Attempt.IsZero() {
		t.Fatalf(
			"direct stream carried attempt %+v",
			client.last.Attempt,
		)
	}
	client.last = Call{Attempt: Attempt{AttemptID: "stale"}}
	if _, err := Generate(
		context.Background(),
		client,
		Input{},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if !client.last.Attempt.IsZero() {
		t.Fatalf("Generate carried attempt %+v", client.last.Attempt)
	}
}
