package llm

import (
	"context"
	"testing"
)

func TestAttemptContext(t *testing.T) {
	if _, ok := AttemptFrom(context.Background()); ok {
		t.Fatal("background context carries an attempt")
	}
	want := Attempt{TurnID: "t1", AttemptID: "t1:model:1"}
	got, ok := AttemptFrom(WithAttempt(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("attempt=%+v ok=%v", got, ok)
	}
}

func TestStreamOutsideKernelHasNoAttempt(t *testing.T) {
	seen := true
	client := &iteratorClient{send: func(ctx context.Context, _ Sink) (Result, error) {
		_, seen = AttemptFrom(ctx)
		return Result{Stop: Stop{Kind: StopFinished}}, nil
	}}
	for range Stream(context.Background(), client, Request{}) {
	}
	if seen {
		t.Fatal("direct stream carried an attempt")
	}
}
