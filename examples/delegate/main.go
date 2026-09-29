// Command delegate runs an agent that hands searches to "explore" subagents
// in the background. The task tool returns at once, so the agent keeps
// working while the subagents search. The host prints each subagent's steps
// as they happen, sends every finished result to the agent, and starts a
// follow-up turn for results that arrive after the turn ended. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// SearchInput is the argument schema for search_notes.
type SearchInput struct {
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
		return fmt.Errorf("delegate: create client: %w", err)
	}
	search, err := newSearchTool()
	if err != nil {
		return fmt.Errorf("delegate: %w", err)
	}

	// The whole wiring: a Tasks value, its tools, and one OnTask callback
	// that sends each finished result to the agent.
	inbox := dashi.NewInbox()
	tasks := delegate.NewTasks(delegate.Config{
		OnChild: printChild,
		OnTask: func(e delegate.TaskEvent) {
			if e.Kind != delegate.TaskEnded {
				return
			}
			fmt.Printf("<< task %s %s\n", e.Task.ID, e.Task.State)
			_, _ = inbox.Send(e.Task.Notice().Steer())
		},
	})
	defer tasks.Close(context.Background())

	recorder := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model: os.Getenv("OPENAI_MODEL"),
			Instructions: "Answer questions about the team notes. Start one " +
				"task with the explore subagent for each separate question, " +
				"all in one step. Do not wait for them: a message arrives " +
				"when each one ends.",
			Limits: dashi.Limits{ModelAttempts: 16},
		},
		Client:   client,
		Recorder: recorder,
		Tools:    tool.Join(tool.List{search}, tasks.Tools()),
	}
	out, err := runner.Run(
		ctx,
		dashi.Turn{},
		dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text(
			"Who owns the billing service, and who is on call for search?",
		))}},
		dashi.WithSteering(inbox),
	)
	if err != nil {
		return fmt.Errorf("delegate: run: %w", err)
	}
	history, err := dashi.Project(out.Turn)
	if err != nil {
		return err
	}

	// Results that ended after the turn wait in the inbox. Each round starts
	// one follow-up turn that carries all of them.
	for {
		left, err := inbox.Leftovers(out.Turn)
		if err != nil {
			return err
		}
		var notices []dashi.Steer
		for _, s := range left {
			if delegate.IsNotice(s) {
				notices = append(notices, s)
			}
		}
		if len(notices) == 0 {
			// Wait covers the gap between a task ending and its notice
			// reaching the inbox, so a true answer means nothing is left.
			idle, err := tasks.Wait(ctx)
			if err != nil {
				return err
			}
			if idle {
				if left, err = inbox.Leftovers(out.Turn); err != nil {
					return err
				}
				if !slices.ContainsFunc(left, delegate.IsNotice) {
					break
				}
			}
			continue
		}
		start := delegate.FollowUp(notices)
		start.History = history
		out, err = runner.Run(
			ctx,
			dashi.Turn{ID: delegate.FollowUpTurnID(notices)},
			start,
			dashi.WithSteering(inbox),
		)
		if err != nil {
			return fmt.Errorf("delegate: follow-up: %w", err)
		}
		if history, err = dashi.Project(out.Turn); err != nil {
			return err
		}
	}
	fmt.Println(out.Answer)
	fmt.Printf("subagents cost %s\n", formatCost(taskSpend(tasks)))
	return nil
}

func taskSpend(tasks *delegate.Tasks) dashi.Spend {
	var total dashi.Spend
	for _, v := range tasks.List() {
		total = total.Add(v.Spend)
	}
	return total
}

// printChild draws each committed child tool call under the task call.
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
func newSearchTool() (tool.Tool, error) {
	search, err := tool.Func(
		"search_notes",
		"Searches the team notes and returns matching lines.",
		func(_ context.Context, _ tool.Call, in SearchInput) (tool.Outcome, error) {
			return tool.Succeeded(
				"ownership.md: billing service - owner: Priya (payments team)",
			), nil
		},
	)
	if err != nil {
		return tool.Tool{}, err
	}
	search.Definition.Annotations.ReadOnly = new(true)
	return search, nil
}

// formatCost prints the cost of a spend as "$0.41". It sums the amounts of
// every currency in the spend, which is correct for the single currency this
// example uses.
func formatCost(s dashi.Spend) string {
	total := 0.0
	for _, amount := range s.Cost {
		f := amount.Float64()
		total += f
	}
	return fmt.Sprintf("$%.2f", total)
}
