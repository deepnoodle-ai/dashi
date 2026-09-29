package delegate_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/delegate"
)

// A background task's result reaches the session as a queued notice.
// Whether it arrives before the first turn ends (Send then runs the queue
// turn itself) or after (RunUntilQuiet waits for it), one queue turn reads
// it, and RunUntilQuiet returns once no task is left. The scripted clients
// need no API key.
func ExampleRunUntilQuiet() {
	ctx := context.Background()
	parent := dashitest.NewClient(
		dashitest.CallTool("c1", delegate.ToolName,
			`{"subagent":"explore","prompt":"Who owns billing?","description":"billing owner"}`),
		dashitest.Reply("I started a search; the answer follows."),
		dashitest.Reply("Priya owns the billing service."),
	)
	child := dashitest.NewClient(dashitest.Reply("ownership.md: billing - Priya"))

	s := &dashi.Session{Agent: dashi.Agent{Model: dashitest.Model}}
	tasks := delegate.NewTasks(delegate.Config{
		Subagents: []delegate.Subagent{{
			Name:        "explore",
			Description: "Searches the notes.",
			Agent:       dashi.Agent{Model: dashitest.Model},
		}},
		Runner: dashi.Runner{Client: child},
		OnTask: func(e delegate.TaskEvent) {
			if e.Kind == delegate.TaskEnded {
				s.EnqueueItem(ctx, e.Task.Notice().Steer().Item)
			}
		},
	})
	defer tasks.Close(ctx)
	s.Runner = dashi.Runner{Client: parent, Tools: tasks.Tools()}

	if _, err := s.Send(ctx, "Who owns billing?"); err != nil {
		fmt.Println(err)
		return
	}
	out, err := delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("turn 2:", out.Turn.ID == dashi.TurnID(s.ID, 2))
	fmt.Println("origin:", out.Origin)
	// Output:
	// Priya owns the billing service.
	// turn 2: true
	// origin: scheduled
}
