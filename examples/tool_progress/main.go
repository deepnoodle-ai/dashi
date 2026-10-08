// Command tool_progress runs a turn whose slow tool reports how far along it
// is, and prints that progress as one line that updates in place. Progress
// is only for display: it is not saved, and the model never sees it. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the default model.
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

	process := newProcessTool()
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
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

	// Stream runs the turn like Run, but yields events as they happen:
	// recorded steps, streamed text, and tool progress, then the result.
	for event, err := range runner.Stream(ctx, dashi.Turn{}, start) {
		if err != nil {
			return err
		}
		switch {
		case event.Progress != nil:
			// Each report replaces the last, so overwrite one line.
			p := event.Progress
			fmt.Printf(
				"\r  %s: %3.0f%% %s",
				p.Name,
				100*p.Done/p.Total,
				p.Message,
			)
		case event.Step != nil && event.Step.Kind == dashi.StepToolFinished:
			fmt.Println()
		case event.Result != nil:
			out := event.Result
			fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
		}
	}
	return nil
}

// newProcessTool simulates a job that processes records one by one. After
// each record it calls tool.Report to say how far it has got.
func newProcessTool() tool.Tool {
	return tool.MustFunc(
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
