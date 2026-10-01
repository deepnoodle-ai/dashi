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
	"github.com/deepnoodle-ai/dashi/session"
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

	s := &session.Session{Agent: dashi.Agent{
		Model: model,
		Instructions: "Start one explore task for the question, then " +
			"answer from its result. Do not answer before the message " +
			"about the task arrives.",
		Limits: dashi.Limits{ModelCalls: 16},
	}}
	ended := make(chan delegate.TaskView, 4)
	tasks := delegate.NewTasks(delegate.Config{OnTask: func(e delegate.TaskEvent) {
		if e.Kind == delegate.TaskEnded {
			_, _ = s.SteerItem(context.Background(), e.Task.Notice().Steer())
			ended <- e.Task
		}
	}})
	t.Cleanup(func() { _ = tasks.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s.Runner = dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    tool.Join(tool.List{search}, tasks.Tools()),
	}
	if _, err := s.Send(ctx,
		"Who owns the billing service? Use a task to look it up."); err != nil {
		t.Fatal(err)
	}
	// A result that arrives after the turn ended starts a queue turn.
	out, err := delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-ended:
		t.Logf("task %s %s: %q", v.ID, v.State, v.Reply)
		if v.State != "completed" || !strings.Contains(v.Reply, "Priya") {
			t.Fatalf("task=%+v", v)
		}
	default:
		t.Fatal("the task never ended")
	}
	t.Logf("answer: %s", out.Reply)
	if !strings.Contains(out.Reply, "Priya") {
		t.Fatalf("the agent never used the result: %q", out.Reply)
	}
}
