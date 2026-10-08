// Command agent runs one agent turn against the OpenAI Responses API and
// prints the reply. Set OPENAI_API_KEY before running it; OPENAI_MODEL
// optionally overrides the default model.
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
		return err
	}
	runner := dashi.Runner{Client: client}
	// Every field may be left empty. An empty Model uses the provider's
	// default model, and an empty Turn gets a generated ID.
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: os.Getenv("OPENAI_MODEL")},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Say hello in one sentence.")),
		},
	})
	if err != nil {
		return err
	}
	// Run returns an error only when the turn could not run at all. Other
	// outcomes, such as a model failure, are in out.Stop.
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}
