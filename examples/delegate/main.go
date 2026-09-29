// Command delegate runs one agent turn in which the model hands a search to
// an "explore" subagent through the task tool. It prints the subagent's
// steps as they happen, indented under the task call, and then the answer and
// the spend split between parent and child. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the catalog's default
// model.
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
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("delegate: create client: %w", err)
	}
	search, err := newSearchTool()
	if err != nil {
		return fmt.Errorf("delegate: %w", err)
	}
	tools := tool.Join(tool.List{search}, delegate.New(delegate.Config{
		// Children inherit the client, recorder, hooks, cancel behavior,
		// and model of the runner below. OnChild draws their steps.
		OnChild: printChild,
	}))

	recorder := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model: os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Answer questions about the team notes. " +
				"Use the task tool with the explore subagent to search.",
		},
		Client:   client,
		Recorder: recorder,
		Tools:    tools,
	}
	out, err := runner.Run(
		ctx,
		dashi.Turn{},
		dashi.Start{Input: []llm.Message{
			llm.UserMessage(llm.Text("Who owns the billing service?")),
		}},
	)
	if err != nil {
		return fmt.Errorf("delegate: run: %w", err)
	}
	fmt.Println(out.Answer)

	report, err := delegate.Report(ctx, recorder, out.TurnView)
	if err != nil {
		return fmt.Errorf("delegate: report: %w", err)
	}
	fmt.Printf(
		"total %s (child agents %s)\n",
		formatCost(report.Inclusive),
		formatCost(report.Children),
	)
	return nil
}

// printChild draws each committed child tool call under the task call.
func printChild(e delegate.ChildEvent) {
	step := e.Event.Step
	if step == nil {
		return
	}
	switch step.Kind {
	case dashi.StepStarted:
		fmt.Printf("  > %s\n", e.Subagent)
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
