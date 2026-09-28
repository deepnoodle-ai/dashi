// Command tool_progress runs one agent turn in which a slow tool reports its
// progress with tool.Report, and renders each snapshot as a live status line
// while the turn streams. Progress is live-only: it is never recorded and
// never reaches the model. Set ANTHROPIC_API_KEY and ANTHROPIC_MODEL before
// running it.
package main

import (
	"context"
	"encoding/json"
	"errors"
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

// ProcessInput is the argument schema for process_dataset.
type ProcessInput struct {
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
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New(
			"tool_progress: set ANTHROPIC_MODEL and ANTHROPIC_API_KEY",
		)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	process, err := newProcessTool()
	if err != nil {
		return fmt.Errorf("tool_progress: %w", err)
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("tool_progress: create client: %w", err)
	}
	runner := dashi.Runner{
		Agent: dashi.Agent{
			ID:       "data-assistant",
			Revision: "1",
			Model:    model,
			Instructions: "You are a data assistant. Use process_dataset " +
				"when asked to process a dataset.",
		},
		Client: client,
		Tools:  tool.List{process},
		// A nil Policy denies every tool call, so allow the local tool.
		Policy: dashi.AllowLocalPolicy{},
	}
	start := dashi.Start{
		CommandID: "process-start",
		Input: []llm.Message{
			llm.UserMessage(llm.Text(
				`Process the dataset named "events-2026" and tell me ` +
					`how many records it had.`,
			)),
		},
	}

	fmt.Println("Asking the agent to process a dataset...")
	turn := dashi.NewTurn("progress-turn")
	for event, err := range runner.Stream(ctx, turn, start) {
		if err != nil {
			return fmt.Errorf("tool_progress: %w", err)
		}
		switch {
		case event.Progress != nil:
			// Each snapshot replaces the last, so overwrite one line.
			fmt.Printf("\r  %s", status(event.Progress))
		case event.Step != nil && event.Step.Kind == dashi.StepToolSettled:
			fmt.Printf("\r%-76s\r", "")
		case event.Outcome != nil:
			out := event.Outcome
			if err := out.RecordingErr(); err != nil {
				return fmt.Errorf("tool_progress: %w", err)
			}
			fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Turn.AnswerText())
		}
	}
	return nil
}

// status renders a snapshot, showing a percentage only when the tool knows
// its total, and reading the structured detail the tool put in Data.
func status(p *dashi.ToolProgress) string {
	var detail struct {
		Bytes     int   `json:"bytes_processed"`
		ElapsedMS int64 `json:"elapsed_ms"`
	}
	_ = json.Unmarshal(p.Data, &detail)
	line := fmt.Sprintf(
		"%s: %s, %d bytes, %d ms",
		p.Name,
		p.Message,
		detail.Bytes,
		detail.ElapsedMS,
	)
	if p.Total > 0 {
		line = fmt.Sprintf("%3.0f%% %s", 100*p.Done/p.Total, line)
	}
	return line
}

// newProcessTool simulates a job that processes records one by one and
// reports a snapshot after each: a status line, counts for a progress bar,
// and structured detail for a custom display.
func newProcessTool() (tool.Tool, error) {
	return tool.Func(
		"process_dataset",
		"Processes the named dataset record by record.",
		func(
			ctx context.Context,
			call tool.Call,
			in ProcessInput,
		) (tool.Outcome, error) {
			const total = 40
			began := time.Now()
			bytes := 0
			for i := 1; i <= total; i++ {
				select {
				case <-time.After(50 * time.Millisecond):
				case <-ctx.Done():
					return tool.Outcome{}, ctx.Err()
				}
				bytes += 1024 + i*13
				data, err := json.Marshal(map[string]any{
					"bytes_processed": bytes,
					"elapsed_ms":      time.Since(began).Milliseconds(),
				})
				if err != nil {
					return tool.Outcome{}, err
				}
				tool.Report(ctx, tool.Progress{
					Message: fmt.Sprintf("processed %d/%d records", i, total),
					Done:    float64(i),
					Total:   float64(total),
					Data:    data,
				})
			}
			return tool.Succeeded(fmt.Sprintf(
				"Processed %d records from %q (%d bytes).",
				total,
				in.Dataset,
				bytes,
			)), nil
		},
	)
}
