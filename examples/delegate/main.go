// Command delegate runs an agent that hands a search to an "explore"
// subagent in the background. The task tool returns at once, so the agent's
// turn ends while the subagent searches. The host prints each subagent's
// steps as they happen. Tasks puts every finished result on the session
// itself: a result reaches the running turn, or, when no turn runs, starts
// a queue turn. RunUntilQuiet runs those turns until every task has ended.
// It needs no API key: scripted dashitest clients stand in for the models,
// so the replies are fixed. With a real client, the session code stays the
// same.
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
	"github.com/deepnoodle-ai/dashi/llm"
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
	// The subagent searches, then answers. Its first reply waits until the
	// parent's turn has ended, as a slow search would, so the result
	// arrives as a queue turn on every run.
	firstTurnDone := make(chan struct{})
	child := &heldClient{
		Client: dashitest.NewClient(
			dashitest.CallTool("search-1", "search_notes", `{"query":"billing owner"}`),
			dashitest.Reply("ownership.md: Priya (payments team) owns the billing service."),
		),
		release: firstTurnDone,
	}

	// The whole wiring: a session, and a Tasks value that delivers to it,
	// with its tools.
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
	fmt.Printf("%s (%s): %s\n", out.Turn.ID, out.Stop.Kind, out.Answer)
	close(firstTurnDone)

	// The result arrives after the turn ended, so it starts a queue turn.
	out, err = delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s, origin %s): %s\n",
		out.Turn.ID, out.Stop.Kind, out.Origin, out.Answer)
	if out.Stop.Kind != dashi.StopCompleted {
		return fmt.Errorf("the last turn stopped %s: %s", out.Stop.Kind, out.Stop.Reason)
	}

	var spend dashi.Spend
	for _, task := range tasks.List() {
		spend = spend.Add(task.Spend)
	}
	fmt.Printf("subagent model calls used %d input tokens\n",
		spend.Usage.TotalInputTokens())
	return nil
}

// printTask shows each task turn start and the notice its end sends to the
// parent.
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
	case dashi.StepStopped:
		fmt.Printf("    %s done\n", e.Subagent)
	}
}

// heldClient answers only once release is closed.
type heldClient struct {
	*dashitest.Client
	release <-chan struct{}
}

func (c *heldClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	select {
	case <-c.release:
	case <-ctx.Done():
		return llm.Result{}, ctx.Err()
	}
	return c.Client.Send(ctx, call)
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
