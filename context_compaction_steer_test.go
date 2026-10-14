package dashi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// steerDuringSummaryClient sends an urgent steer while the first summary
// call runs, then gives the listener a moment to cut the call before it
// answers.
type steerDuringSummaryClient struct {
	*activeClient
	inbox *watchedInbox
	t     *testing.T
	sent  bool
}

func (c *steerDuringSummaryClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if !c.sent && strings.HasPrefix(call.Request.Snapshot().Input.System, activeSummaryPrompt) {
		c.sent = true
		c.inbox.sendUrgent(c.t, "also check the logs")
		select {
		case <-ctx.Done():
			return llm.Result{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return c.activeClient.Send(ctx, call)
}

// An urgent steer that arrives while the turn summarizes its context waits
// for the ordinary call after the summary. Cutting the summary call would
// leave the turn over its window with its summary spent.
func TestActiveCompactionUrgentSteerWaitsForSummary(t *testing.T) {
	c, a := activeFixture()
	inbox := newWatchedInbox()
	client := &steerDuringSummaryClient{activeClient: c, inbox: inbox, t: t}
	calls := 0
	out, err := (Runner{Client: client, Tools: activeTools(&calls)}).Run(t.Context(), Turn{ID: "steer"}, activeStart(a), WithInbox(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if !client.sent {
		t.Fatal("the summary call never ran")
	}
	if out.Stop.Kind != StopCompleted || len(out.ContextCompactions) != 2 {
		t.Fatalf("stop=%+v summaries=%d", out.Stop, len(out.ContextCompactions))
	}
	for _, f := range finishes(out.Turn) {
		if f.Interrupted != InterruptNone {
			t.Fatalf("call %s interrupted by %s", f.ModelCallID, f.Interrupted)
		}
	}
	// The steer is recorded after the first summary is applied.
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
}
