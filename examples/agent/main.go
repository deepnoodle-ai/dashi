// Command agent runs one agent turn against the OpenAI Responses API and
// prints the answer. Set OPENAI_API_KEY before running it; OPENAI_MODEL
// optionally overrides the catalog's default model.
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

	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("agent: create client: %w", err)
	}
	// Every Agent field may be left empty. An empty Model is the provider's
	// catalog default; the turn and command IDs are generated.
	runner := dashi.Runner{
		Agent:  dashi.Agent{Model: os.Getenv("OPENAI_MODEL")},
		Client: client,
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Say hello in one sentence.")),
		},
	})
	if err != nil {
		return fmt.Errorf("agent: run: %w", err)
	}
	// Run's error means the command did not apply. A model failure, a limit,
	// or a wait is a result instead: Stop says how the turn ended.
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}
