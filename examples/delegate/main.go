// Command delegate runs an agent that hands searches to "explore" subagents
// in the background. The task tool returns at once, so the agent keeps
// working while the subagents search. The host prints each subagent's steps
// as they happen, sends every finished result to the agent, and starts a
// follow-up turn for results that arrive after the turn ended. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// catalog's default model.
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
		return fmt.Errorf("delegate: create client: %w", err)
	}
	search, err := newSearchTool()
	if err != nil {
		return fmt.Errorf("delegate: create tool: %w", err)
	}

	// The whole wiring: a Tasks value, its tools, and one OnTask callback
	// that sends each finished result to the agent through the inbox.
	inbox := dashi.NewInbox()
	tasks := delegate.NewTasks(delegate.Config{
		OnChild: printChild,
		OnTask: func(e delegate.TaskEvent) {
			if e.Kind == delegate.TaskEnded {
				fmt.Printf("<< task %s %s\n", e.Task.ID, e.Task.State)
				_, _ = inbox.Send(e.Task.Notice().Steer())
			}
		},
	})
	defer tasks.Close(context.Background())

	agent := dashi.Agent{
		Model: os.Getenv("OPENAI_MODEL"),
		Instructions: "Answer questions about the team notes. Start one " +
			"task with the explore subagent for each separate question, " +
			"all in one step. Do not wait for them: a message arrives " +
			"when each one ends.",
		Limits: dashi.Limits{ModelAttempts: 16},
	}
	runner := dashi.Runner{
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    tool.Join(tool.List{search}, tasks.Tools()),
	}
	out, err := runner.Run(
		ctx,
		dashi.Turn{},
		dashi.Start{
			Agent: agent,
			Input: []llm.Message{llm.UserMessage(llm.Text(
				"Who owns the billing service, and who is on call for search?",
			))},
		},
		dashi.WithSteering(inbox),
	)
	if err != nil {
		return fmt.Errorf("delegate: run: %w", err)
	}

	// Results that ended after the turn wait in the inbox. Each round starts
	// one follow-up turn that carries all of them, with the conversation so
	// far as its history.
	var history []llm.Message
	for {
		notices, err := waitingNotices(ctx, inbox, tasks, out.Turn)
		if err != nil {
			return fmt.Errorf("delegate: %w", err)
		}
		if len(notices) == 0 {
			break
		}
		turn, err := dashi.Project(out.Turn)
		if err != nil {
			return fmt.Errorf("delegate: %w", err)
		}
		history = append(history, turn...)
		start := delegate.FollowUp(agent, notices)
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

// waitingNotices returns the task results the turn did not deliver, waiting
// while tasks still run. It returns none once every task has ended and been
// delivered. Wait covers the gap between a task ending and its notice
// reaching the inbox, so an empty inbox after an idle Wait means nothing is
// left.
func waitingNotices(
	ctx context.Context,
	inbox *dashi.Inbox,
	tasks *delegate.Tasks,
	turn dashi.Turn,
) ([]dashi.Steer, error) {
	idle := false
	for {
		left, err := inbox.Leftovers(turn)
		if err != nil {
			return nil, err
		}
		notices := slices.DeleteFunc(left, func(s dashi.Steer) bool {
			return !delegate.IsNotice(s)
		})
		if len(notices) > 0 || idle {
			return notices, nil
		}
		if idle, err = tasks.Wait(ctx); err != nil {
			return nil, err
		}
	}
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
func newSearchTool() (tool.Tool, error) {
	search, err := tool.Func(
		"search_notes",
		"Searches the team notes and returns matching lines.",
		func(_ context.Context, _ tool.Call, _ searchArgs) (tool.Outcome, error) {
			return tool.Succeeded(
				"ownership.md: billing service - owner: Priya (payments team)",
			), nil
		},
		tool.ReadOnly(),
	)
	if err != nil {
		return tool.Tool{}, err
	}
	return search, nil
}
