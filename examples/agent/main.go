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

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
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
	def := kernel.Definition{
		ID:       "example",
		Revision: "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    model,
	}
	start := kernel.Start{
		ID: "example-start",
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Say hello in one sentence.")),
		},
	}
	bindings := kernel.Bindings{
		Client:   client,
		Recorder: kernel.NewMemoryRecorder(),
	}

	var engine kernel.Engine
	turn := kernel.Turn{ID: "example-turn"}
	outcome, err := engine.Apply(ctx, turn, def, start, bindings)
	if err != nil {
		return fmt.Errorf("agent: apply start: %w", err)
	}
	// Apply returns an error only for an invalid command. A failed recorder
	// write is reported in the outcome, so check it separately.
	if outcome.Persistence != kernel.PersistenceAcknowledged {
		return fmt.Errorf(
			"agent: recording %s: %v",
			outcome.Persistence,
			outcome.WriteError,
		)
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
	return nil
}
