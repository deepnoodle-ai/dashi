package llm

import (
	"context"
	"errors"
	"testing"
)

type iteratorClient struct {
	sends int
	send  func(context.Context, Sink) (Result, error)
}

func (*iteratorClient) Provider() string                                { return "test" }
func (*iteratorClient) API() string                                     { return "test" }
func (*iteratorClient) ModelInfo(string) (ModelInfo, bool)              { return ModelInfo{}, false }
func (*iteratorClient) Prepare(context.Context, Input) (Request, error) { return Request{}, nil }
func (c *iteratorClient) Send(ctx context.Context, _ Request, sink Sink) (Result, error) {
	c.sends++
	return c.send(ctx, sink)
}

func TestStreamIsLazyBackpressuredAndTerminal(t *testing.T) {
	afterFirst := false
	client := &iteratorClient{send: func(_ context.Context, sink Sink) (Result, error) {
		sink(Delta{Kind: DeltaText, Text: "one"})
		afterFirst = true
		sink(Delta{Kind: DeltaText, Text: "two"})
		return Result{ID: "result", Stop: Stop{Kind: StopFinished}}, nil
	}}
	sequence := Stream(context.Background(), client, Request{})
	if client.sends != 0 {
		t.Fatal("constructing iterator dispatched request")
	}
	var deltas []string
	terminals := 0
	for event, err := range sequence {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil {
			if event.Delta.Text == "one" && afterFirst {
				t.Fatal("producer ran past first yield")
			}
			deltas = append(deltas, event.Delta.Text)
		}
		if event.Result != nil {
			terminals++
			if event.Result.ID != "result" || event.Delta != nil {
				t.Fatalf("terminal=%+v", event)
			}
		}
	}
	if client.sends != 1 || len(deltas) != 2 || deltas[0] != "one" || deltas[1] != "two" || terminals != 1 {
		t.Fatalf("sends=%d deltas=%v terminals=%d", client.sends, deltas, terminals)
	}
	for _, err := range sequence {
		if !errors.Is(err, ErrStreamConsumed) {
			t.Fatalf("second range error=%v", err)
		}
	}
	if client.sends != 1 {
		t.Fatal("second range repeated model dispatch")
	}
}

func TestStreamBreakCancelsWithoutTerminal(t *testing.T) {
	canceled := false
	client := &iteratorClient{send: func(ctx context.Context, sink Sink) (Result, error) {
		sink(Delta{Kind: DeltaText, Text: "first"})
		<-ctx.Done()
		canceled = true
		return Result{ID: "partial"}, ctx.Err()
	}}
	seen := 0
	for event, err := range Stream(context.Background(), client, Request{}) {
		if err != nil || event.Delta == nil {
			t.Fatalf("unexpected event=%+v err=%v", event, err)
		}
		seen++
		break
	}
	if !canceled || seen != 1 || client.sends != 1 {
		t.Fatalf("canceled=%t seen=%d sends=%d", canceled, seen, client.sends)
	}
}

func TestStreamReturnsPartialResultWithError(t *testing.T) {
	failure := errors.New("stream failed")
	client := &iteratorClient{send: func(_ context.Context, sink Sink) (Result, error) {
		sink(Delta{Kind: DeltaText, Text: "partial"})
		return Result{ID: "partial", Stop: Stop{Kind: StopIncomplete}}, failure
	}}
	var final StreamEvent
	var finalErr error
	for event, err := range Stream(context.Background(), client, Request{}) {
		if event.Result != nil {
			final, finalErr = event, err
		}
	}
	if final.Result == nil || final.Result.ID != "partial" || !errors.Is(finalErr, failure) {
		t.Fatalf("terminal=%+v err=%v", final, finalErr)
	}
}
