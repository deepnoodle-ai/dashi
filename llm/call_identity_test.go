package llm

import (
	"context"
	"testing"
)

func TestAttemptIsZero(t *testing.T) {
	if !(CallIdentity{}).IsZero() {
		t.Fatal("empty attempt is not zero")
	}
	if (CallIdentity{TurnID: "t1", ModelCallID: "t1:model:1"}).IsZero() {
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
	want := CallIdentity{TurnID: "t1", ModelCallID: "t1:model:1"}
	var observed []string
	for range Stream(
		t.Context(),
		client,
		Call{
			Identity: want,
			Sink: func(d Delta) {
				observed = append(
					observed,
					d.Text,
				)
			},
		},
	) {
	}
	if client.last.Identity != want {
		t.Fatalf("attempt=%+v, want %+v", client.last.Identity, want)
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
	for range Stream(t.Context(), client, Call{}) {
	}
	if !client.last.Identity.IsZero() {
		t.Fatalf(
			"direct stream carried attempt %+v",
			client.last.Identity,
		)
	}
	client.last = Call{Identity: CallIdentity{ModelCallID: "stale"}}
	if _, err := Generate(
		t.Context(),
		client,
		Input{},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if !client.last.Identity.IsZero() {
		t.Fatalf("Generate carried attempt %+v", client.last.Identity)
	}
}
