//go:build live

package delegate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Run with DASHI_LIVE_AGENT_TEST=1, OPENAI_API_KEY, and OPENAI_MODEL. The
// test makes a few paid calls.
func TestLiveBackgroundTask(t *testing.T) {
	key, model := providertest.AgentCredentials(t, "OPENAI")
	client, err := responses.New(responses.Config{
		APIKey:           key,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkLeaks(t)
	search, err := tool.Func(
		"search_notes",
		"Searches the team notes and returns matching lines.",
		func(_ context.Context, _ tool.Call, in struct {
			Query string `json:"query" description:"Words to look for"`
		}) (tool.Outcome, error) {
			return tool.Succeeded(
				"ownership.md: the billing service is owned by Priya"), nil
		},
		tool.ReadOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}

	inbox := dashi.NewInbox()
	ended := make(chan delegate.TaskView, 4)
	tasks := delegate.NewTasks(delegate.Config{OnTask: func(e delegate.TaskEvent) {
		if e.Kind == delegate.TaskEnded {
			_, _ = inbox.Send(e.Task.Notice().Steer())
			ended <- e.Task
		}
	}})
	t.Cleanup(func() { _ = tasks.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	agent := dashi.Agent{
		Model: model,
		Instructions: "Start one explore task for the question, then " +
			"answer from its result. Do not answer before the message " +
			"about the task arrives.",
		Limits: dashi.Limits{ModelAttempts: 16},
	}
	runner := dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    tool.Join(tool.List{search}, tasks.Tools()),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text(
			"Who owns the billing service? Use a task to look it up."))},
	}, dashi.WithSteering(inbox))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-ended:
		t.Logf("task %s %s: %q", v.ID, v.State, v.Answer)
		if v.State != "completed" || !strings.Contains(v.Answer, "Priya") {
			t.Fatalf("task=%+v", v)
		}
	case <-time.After(time.Minute):
		t.Fatal("the task never ended")
	}
	turn := out.Turn
	if !strings.Contains(out.Answer, "Priya") {
		// The turn ended before the result arrived: the notice is owed.
		left, err := inbox.Leftovers(turn)
		if err != nil {
			t.Fatal(err)
		}
		var notices []dashi.Steer
		for _, s := range left {
			if delegate.IsNotice(s) {
				notices = append(notices, s)
			}
		}
		if len(notices) != 1 {
			t.Fatalf("answer=%q and %d notices owed", out.Answer, len(notices))
		}
		history, err := dashi.Project(turn)
		if err != nil {
			t.Fatal(err)
		}
		start := delegate.FollowUp(agent, notices)
		start.History = history
		out, err = runner.Run(ctx, dashi.Turn{ID: delegate.FollowUpTurnID(notices)}, start)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("answer: %s", out.Answer)
	if !strings.Contains(out.Answer, "Priya") {
		t.Fatalf("the agent never used the result: %q", out.Answer)
	}
}
