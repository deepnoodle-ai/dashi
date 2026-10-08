// Command reasoning asks a model to think before it answers, and streams a
// summary of its thinking apart from its reply. Some replies come with no
// summary. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL
// optionally overrides the model, claude-sonnet-4-6.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	// Effort sets how much the model thinks: "low", "medium", or "high".
	// ReasoningSummary asks for a readable summary of that thinking.
	request, err := client.Prepare(ctx, llm.Input{
		Model:  cmp.Or(os.Getenv("ANTHROPIC_MODEL"), "claude-sonnet-4-6"),
		Stream: true,
		Settings: llm.Settings{
			Reasoning:        &llm.ReasoningSettings{Effort: "medium"},
			ReasoningSummary: new(true),
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Is 391 prime? Answer in one sentence.")),
		},
	})
	if err != nil {
		return err
	}
	// Each streamed piece (a delta) says whether it is thinking or reply
	// text, so the two can be printed apart.
	labels := map[llm.DeltaKind]string{
		llm.DeltaReasoning: "thinking",
		llm.DeltaText:      "answer",
	}
	var last llm.DeltaKind
	var result llm.Result
	for event, err := range llm.Stream(ctx, client, llm.Call{Request: request}) {
		if err != nil {
			fmt.Println()
			return err
		}
		if d := event.Delta; d != nil && labels[d.Kind] != "" {
			if d.Kind != last {
				fmt.Printf("\n--- %s ---\n", labels[d.Kind])
				last = d.Kind
			}
			fmt.Print(d.Text)
		}
		if event.Result != nil {
			result = *event.Result
		}
	}
	fmt.Printf("\n\nStop: %s; model: %s\n", result.Stop.Kind, result.Served.Model)
	if u := result.Usage; u != nil {
		fmt.Printf("Tokens: %d in, %d out (thinking counts as output)\n",
			u.InputTokens, u.OutputTokens)
	}
	for _, w := range result.Warnings {
		fmt.Printf("Warning (%s): %s\n", w.Code, w.Message)
	}
	return nil
}
