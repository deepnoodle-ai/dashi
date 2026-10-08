package dashitest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func send(ctx context.Context, c *dashitest.Client) (llm.Result, error) {
	return llm.Generate(ctx, c, llm.Input{
		Model:    dashitest.Model,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}, nil)
}

func TestClientHoldWaitsForRelease(t *testing.T) {
	c := dashitest.NewClient(dashitest.Reply("done"))
	release := make(chan struct{})
	c.Hold(release)
	got := make(chan string, 1)
	go func() {
		result, err := send(t.Context(), c)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- result.Message.Text()
	}()
	select {
	case text := <-got:
		t.Fatalf("Send answered %q before release", text)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if text := <-got; text != "done" {
		t.Fatalf("reply = %q, want done", text)
	}
}

func TestClientHoldEndsWithContext(t *testing.T) {
	c := dashitest.NewClient(dashitest.Reply("done"))
	c.Hold(make(chan struct{}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := send(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send error = %v, want context.Canceled", err)
	}
	if n := len(c.Inputs()); n != 0 {
		t.Fatalf("canceled Send recorded %d inputs", n)
	}
}
