package dashi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// steerDuringSummaryClient runs onSummary at the start of each summary
// call, numbered from 1. An error it returns becomes the call's error.
// Otherwise the first summary call waits a moment before it answers, so an
// urgent-steer listener has time to cut it.
type steerDuringSummaryClient struct {
	*activeClient
	onSummary func(ctx context.Context, n int) error
	summaries int
}

func (c *steerDuringSummaryClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if strings.HasPrefix(call.Request.Snapshot().Input.System, activeSummaryPrompt) {
		c.summaries++
		if c.onSummary != nil {
			if err := c.onSummary(ctx, c.summaries); err != nil {
				return llm.Result{}, err
			}
		}
		if c.summaries == 1 {
			select {
			case <-ctx.Done():
				return llm.Result{}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return c.activeClient.Send(ctx, call)
}

// retryFixture is activeFixture whose first summary is cut off at its
// output limit, so the turn retries it.
func retryFixture() (*activeClient, Agent) {
	c, a := activeFixture()
	c.results = append(c.results[:2], append([]llm.Result{outputLimitedSummary()}, c.results[2:]...)...)
	return c, a
}

// checkSteerAfterSummary checks that the turn completed with both
// summaries, cut no call, recorded the steer after the first summary, and
// sent the steer's text to no summary call made before that.
func checkSteerAfterSummary(t *testing.T, out RunResult, inputs []llm.Input, text string) {
	t.Helper()
	if out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 2 {
		t.Fatalf("stop=%+v summaries=%d", out.Stop, len(out.ContextCompactions))
	}
	for _, f := range finishes(out.Turn) {
		if f.Interrupted != InterruptNone {
			t.Fatalf("call %s interrupted by %s", f.ModelCallID, f.Interrupted)
		}
	}
	summary, steer := -1, -1
	for i, step := range out.Turn.Steps {
		if step.Kind == StepContextCompacted && summary < 0 {
			summary = i
		}
		if step.Kind == StepMessagesAdded && step.Context != nil && step.Context.Steering {
			steer = i
		}
	}
	if steer < 0 || summary < 0 || steer < summary {
		t.Fatalf("summary at %d, steer at %d: want the steer after the summary", summary, steer)
	}
	// The summary calls up to the first ordinary call after them.
	summarized := false
	for _, in := range inputs {
		if !strings.HasPrefix(in.System, activeSummaryPrompt) {
			if summarized {
				break
			}
			continue
		}
		summarized = true
		for _, m := range in.Messages {
			if strings.Contains(m.Text(), text) {
				t.Fatalf("a summary call carried the steer %q", text)
			}
		}
	}
}

// An urgent steer that arrives while the turn summarizes its context waits
// for the ordinary call after the summary. Cutting the summary call would
// leave the turn over its window with its summary spent.
func TestActiveCompactionUrgentSteerWaitsForSummary(t *testing.T) {
	c, a := activeFixture()
	inbox := newWatchedInbox()
	client := &steerDuringSummaryClient{activeClient: c}
	client.onSummary = func(_ context.Context, n int) error {
		if n == 1 {
			inbox.sendUrgent(t, "also check the logs")
		}
		return nil
	}
	calls := 0
	out, err := (Runner{Client: client, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "steer"}, activeStart(a), WithInbox(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if client.summaries == 0 {
		t.Fatal("the summary call never ran")
	}
	checkSteerAfterSummary(t, out, c.sent, "also check the logs")
}

// A normal steer that arrives during a summary call reaches the ordinary
// call after the summary. When the summary is retried, the retry does not
// take the steer either.
func TestActiveCompactionSteerWaitsForSummary(t *testing.T) {
	for name, fixture := range map[string]func() (*activeClient, Agent){
		"summary": activeFixture,
		"retry":   retryFixture,
	} {
		t.Run(name, func(t *testing.T) {
			c, a := fixture()
			inbox := newWatchedInbox()
			client := &steerDuringSummaryClient{activeClient: c}
			client.onSummary = func(_ context.Context, n int) error {
				if n == 1 {
					if _, err := inbox.SendText("also check the logs"); err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			calls := 0
			out, err := (Runner{Client: client, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "steer"}, activeStart(a), WithInbox(inbox))
			if err != nil {
				t.Fatal(err)
			}
			checkSteerAfterSummary(t, out, c.sent, "also check the logs")
		})
	}
}

// A steer waiting when a summary call fails and the turn stops is not
// lost: the turn does not record it, and the inbox hands it back as a
// leftover for the next turn. Here the steer arrives during the first
// summary call, and the run is cancelled during the retry.
func TestActiveCompactionStoppedSummaryLeavesSteer(t *testing.T) {
	c, a := retryFixture()
	inbox := newWatchedInbox()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &steerDuringSummaryClient{activeClient: c}
	client.onSummary = func(ctx context.Context, n int) error {
		switch n {
		case 1:
			if _, err := inbox.SendText("also check the logs"); err != nil {
				t.Error(err)
			}
		case 2:
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	calls := 0
	out, _ := (Runner{Client: client, Tools: activeTools(&calls)}).Run(ctx, Turn{ID: "steer"}, activeStart(a), WithInbox(inbox))
	if client.summaries != 2 || out.Stop.Kind == StopCompleted {
		t.Fatalf("summaries=%d stop=%+v", client.summaries, out.Stop)
	}
	for _, step := range out.Turn.Steps {
		if step.Kind == StepMessagesAdded && step.Context != nil && step.Context.Steering {
			t.Fatal("the stopped turn recorded the steer")
		}
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Item.Message.Text() != "also check the logs" {
		t.Fatalf("leftovers = %+v", left)
	}
}
