// Command tool_progress runs one agent turn in which a slow tool reports its
// progress with tool.Report, and renders each snapshot as a live status line
// while the turn streams. Progress is live-only: it is never recorded and
// never reaches the model. Set ANTHROPIC_API_KEY before running it;
// ANTHROPIC_MODEL optionally overrides the catalog's default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type processArgs struct {
	Dataset string `json:"dataset" description:"Name of the dataset to process"`
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

	process, err := newProcessTool()
	if err != nil {
		return fmt.Errorf("tool_progress: create tool: %w", err)
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("tool_progress: create client: %w", err)
	}
	runner := dashi.Runner{Client: client, Tools: tool.List{process}}
	start := dashi.Start{
		Agent: dashi.Agent{
			Model: os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a data assistant. Use process_dataset " +
				"when asked to process a dataset.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text(
				`Process the dataset named "events-2026" and tell me ` +
					`how many records it had.`,
			)),
		},
	}

	// Stream runs the turn like Run, yielding committed steps, model text
	// deltas, and tool progress as they happen, then the Outcome.
	for event, err := range runner.Stream(ctx, dashi.Turn{}, start) {
		if err != nil {
			return fmt.Errorf("tool_progress: run: %w", err)
		}
		switch {
		case event.Progress != nil:
			// Each snapshot replaces the last, so overwrite one line.
			p := event.Progress
			fmt.Printf(
				"\r  %s: %3.0f%% %s",
				p.Name,
				100*p.Done/p.Total,
				p.Message,
			)
		case event.Step != nil && event.Step.Kind == dashi.StepToolSettled:
			fmt.Println()
		case event.Outcome != nil:
			out := event.Outcome
			fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
		}
	}
	return nil
}

// newProcessTool simulates a job that processes records one by one and
// reports a snapshot after each. Total is known here; a tool that cannot
// know it leaves Total zero and reports only Message.
func newProcessTool() (tool.Tool, error) {
	return tool.Func(
		"process_dataset",
		"Processes the named dataset record by record.",
		func(ctx context.Context, _ tool.Call, in processArgs) (tool.Outcome, error) {
			const total = 40
			for i := 1; i <= total; i++ {
				select {
				case <-time.After(50 * time.Millisecond):
				case <-ctx.Done():
					return tool.Outcome{}, ctx.Err()
				}
				tool.Report(ctx, tool.Progress{
					Message: fmt.Sprintf("processed %d/%d records", i, total),
					Done:    float64(i),
					Total:   float64(total),
				})
			}
			return tool.Succeeded(fmt.Sprintf(
				"Processed %d records from %q.",
				total,
				in.Dataset,
			)), nil
		},
	)
}
