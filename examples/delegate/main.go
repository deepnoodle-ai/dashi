// Command delegate shows an agent handing a search to a subagent, a second
// agent that runs in the background. The agent starts the task and ends its
// turn without waiting. When the subagent's answer comes back, the session
// starts a new turn so the agent can pass it on; delegate.RunUntilQuiet runs
// such turns until no task is left.
//
// It needs no API key: scripted dashitest clients stand in for the models, so
// the replies are fixed. With a real client the code stays the same.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/session"
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
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// The scripts are what the models would answer. The parent starts a
	// task, ends its turn, and answers once the task's result arrives.
	parent := dashitest.NewClient(
		dashitest.CallTool("call-1", delegate.ToolName,
			`{"subagent":"explore","description":"billing owner",`+
				`"prompt":"Find who owns the billing service."}`),
		dashitest.Reply("An explore task is looking; I will answer when it reports."),
		dashitest.Reply("Priya, on the payments team, owns the billing service."),
	)
	// The subagent searches, then answers. Hold keeps its replies back
	// until the parent's first turn has ended, as a slow search would.
	child := dashitest.NewClient(
		dashitest.CallTool("search-1", "search_notes", `{"query":"billing owner"}`),
		dashitest.Reply("ownership.md: Priya (payments team) owns the billing service."),
	)
	firstTurnDone := make(chan struct{})
	child.Hold(firstTurnDone)

	// A session holds the conversation. Tasks runs subagents for it, gives
	// the agent the tools to start them, and sends each finished answer back
	// to the session.
	s := &session.Session{
		ID:  "notes",
		New: true,
		Agent: dashi.Agent{
			Model: dashitest.Model,
			Instructions: "Answer questions about the team notes. Start a " +
				"task with the explore subagent for each question. Do not " +
				"wait for it: a message arrives when it ends.",
		},
	}
	tasks := delegate.NewTasks(delegate.Config{
		Session: s,
		Runner:  dashi.Runner{Client: child},
		OnChild: printChild,
		OnTask:  printTask,
	})
	defer tasks.Close(context.Background())
	s.Runner = dashi.Runner{
		Client:   parent,
		Recorder: dashi.NewMemoryRecorder(),
		Tools:    tool.Join(tool.List{newSearchTool()}, tasks.Tools()),
	}

	question := "Who owns the billing service?"
	fmt.Println("> " + question)
	out, err := s.Send(ctx, question)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s): %s\n", out.Turn.ID, out.Stop.Kind, out.Reply)
	close(firstTurnDone)

	// The answer arrives after that turn ended, so the session starts a new
	// turn for it.
	out, err = delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s, origin %s): %s\n",
		out.Turn.ID, out.Stop.Kind, out.Origin, out.Reply)
	if out.Stop.Kind != dashi.StopCompleted {
		return fmt.Errorf("the last turn stopped %s: %s", out.Stop.Kind, out.Stop.Reason)
	}

	return nil
}

// printTask shows when each task starts, and the message its answer becomes
// for the parent agent.
func printTask(e delegate.TaskEvent) {
	switch e.Kind {
	case delegate.TaskStarted:
		fmt.Printf("  task %s started (%s)\n", e.Task.ID, e.Task.Subagent)
	case delegate.TaskEnded:
		fmt.Printf("  task %s %s; the parent gets:\n", e.Task.ID, e.Task.State)
		for line := range strings.Lines(e.Task.Notice().Text()) {
			fmt.Printf("    | %s", line)
		}
		fmt.Println()
	}
}

// printChild draws each subagent's tool calls under its task.
func printChild(e delegate.ChildEvent) {
	step := e.Event.Step
	if step == nil {
		return
	}
	switch step.Kind {
	case dashi.StepToolStarted:
		fmt.Printf("    %s %s\n", step.Tool.Call.Name, step.Tool.Call.Args)
	case dashi.StepTurnStopped:
		fmt.Printf("    %s done\n", e.Subagent)
	}
}

// newSearchTool returns a read-only tool over canned notes. The read-only
// annotation is what lets the explore subagent use it.
func newSearchTool() tool.Tool {
	return tool.MustFunc("search_notes", "Searches the team notes and returns matching lines.",
		func(context.Context, tool.Call, searchArgs) (tool.Outcome, error) {
			return tool.Succeeded("ownership.md: billing service - owner: Priya (payments team)"), nil
		},
		tool.ReadOnly(),
	)
}
