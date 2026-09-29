// Command delegate runs an agent that hands searches to "explore" subagents
// in the background. The task tool returns at once, so the agent keeps
// working while the subagents search. The host prints each subagent's steps
// as they happen. Tasks puts every finished result on the session itself: a
// result reaches the running turn, or, when no turn runs, starts a queue
// turn. RunUntilQuiet runs those turns until every task has ended. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// catalog's default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

type searchArgs struct {
	Query string `json:"query" description:"Words to look for"`
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	client, err := responses.New(responses.Config{})
	if err != nil {
		return err
	}
	search := newSearchTool()

	// The whole wiring: a session, and a Tasks value that delivers to it,
	// with its tools.
	s := &dashi.Session{
		Agent: dashi.Agent{
			Model: os.Getenv("OPENAI_MODEL"),
			Instructions: "Answer questions about the team notes. Start one " +
				"task with the explore subagent for each separate " +
				"question, all in one step. Do not wait for them: a " +
				"message arrives when each one ends.",
			Limits: dashi.Limits{ModelAttempts: 16},
		},
	}
	tasks := delegate.NewTasks(delegate.Config{
		Session: s,
		OnChild: printChild,
	})
	defer tasks.Close(context.Background())
	s.Runner = dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    tool.Join(tool.List{search}, tasks.Tools()),
	}

	out, err := s.Send(ctx,
		"Who owns the billing service, and who is on call for search?")
	if err != nil {
		return err
	}
	// Results that arrive after the turn ended start queue turns.
	out, err = delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)

	var spend dashi.Spend
	for _, task := range tasks.List() {
		spend = spend.Add(task.Spend)
	}
	for currency, amount := range spend.Cost {
		fmt.Printf("Subagent cost: %s %s\n", amount, currency)
	}
	return nil
}

// printChild draws each subagent's tool calls under its task.
func printChild(e delegate.ChildEvent) {
	step := e.Event.Step
	if step == nil {
		return
	}
	switch step.Kind {
	case dashi.StepStarted:
		fmt.Printf("  > %s starts\n", e.Subagent)
	case dashi.StepToolStarted:
		fmt.Printf("      %s %s\n", step.Tool.Call.Name, step.Tool.Call.Args)
	case dashi.StepStopped:
		fmt.Printf("  < %s done\n", e.Subagent)
	}
}

// newSearchTool returns a read-only tool over canned notes. The read-only
// annotation is what lets the explore subagent use it.
func newSearchTool() tool.Tool {
	return tool.MustFunc(
		"search_notes",
		"Searches the team notes and returns matching lines.",
		func(_ context.Context, _ tool.Call, _ searchArgs) (tool.Outcome, error) {
			return tool.Succeeded(
				"ownership.md: billing service - owner: Priya (payments team)",
			), nil
		},
		tool.ReadOnly(),
	)
}
