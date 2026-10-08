// Command basic makes one model call without an agent and prints the reply
// as it streams in. Notice that streaming needs no extra machinery:
// llm.Generate calls a function you pass with each piece of text as it
// arrives, then returns the whole reply.
//
// Set OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides
// the catalog's small OpenAI model.
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
		return err
	}
	// Stream asks the provider to send the reply in pieces. Generate calls
	// the sink with each piece (a delta) and then returns the whole result.
	input := llm.Input{
		Model:    model,
		Stream:   true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("Say hello in one sentence."))},
	}
	printText := func(delta llm.Delta) {
		if delta.Kind == llm.DeltaText {
			fmt.Print(delta.Text)
		}
	}
	result, err := llm.Generate(ctx, client, input, printText)
	if err != nil {
		return err
	}
	fmt.Printf("\nStop: %s; served model: %s\n", result.Stop.Kind, result.Served.Model)
	for _, w := range result.Warnings {
		fmt.Printf("Warning (%s): %s\n", w.Code, w.Message)
	}
	return nil
}
