// Command basic makes one streamed model call without an agent. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// catalog's small OpenAI model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
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
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// The llm layer never picks a model; the catalog names one per provider.
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model, _ = catalog.Small("openai")
	}
	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("basic: create client: %w", err)
	}
	// Prepare validates the input and builds the request without sending
	// it; Stream sends it and yields text deltas, then the final Result.
	request, err := client.Prepare(ctx, llm.Input{
		Model:  model,
		Stream: true,
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Say hello in one sentence.")),
		},
	})
	if err != nil {
		return fmt.Errorf("basic: prepare: %w", err)
	}
	var result llm.Result
	for event, err := range llm.Stream(ctx, client, llm.Call{Request: request}) {
		if err != nil {
			return fmt.Errorf("basic: stream: %w", err)
		}
		if event.Delta != nil && event.Delta.Kind == llm.DeltaText {
			fmt.Print(event.Delta.Text)
		}
		if event.Result != nil {
			result = *event.Result
		}
	}
	fmt.Printf(
		"\nStop: %s; served model: %s\n",
		result.Stop.Kind,
		result.Served.Model,
	)
	for _, w := range result.Warnings {
		fmt.Printf("Warning (%s): %s\n", w.Code, w.Message)
	}
	return nil
}
