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
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Say hello in one sentence.")),
		},
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if outcome.Stop.Kind != dashi.StopCompleted {
		return fmt.Errorf(
			"agent: turn stopped %s: %s",
			outcome.Stop.Kind,
			outcome.Stop.Reason,
		)
	}
	fmt.Println(outcome.Turn.AnswerText())
	return nil
}
