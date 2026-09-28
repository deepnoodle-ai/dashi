// Command agent runs one recorded turn against the OpenAI Responses API and
// prints the answer. Set OPENAI_API_KEY and OPENAI_MODEL before running it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("agent: set OPENAI_MODEL and OPENAI_API_KEY")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("agent: create client: %w", err)
	}
	// Provider and API are left empty; the runner fills them from client.
	runner := dashi.Runner{
		Agent: dashi.Agent{
			ID:       "example",
			Revision: "1",
			Model:    model,
		},
		Client:   client,
		Recorder: dashi.NewMemoryRecorder(),
	}
	outcome, err := runner.Run(
		ctx,
		dashi.NewTurn("example-turn"),
		dashi.Start{
			CommandID: "example-start",
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Say hello in one sentence.")),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("agent: run start: %w", err)
	}
	// Run returns an error only for an invalid command. A failed recorder
	// write is reported in the outcome, so check it separately.
	if err := outcome.RecordingErr(); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
	return nil
}
