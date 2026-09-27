// Command basic makes one model call without an agent or session. Set
// OPENAI_API_KEY and OPENAI_MODEL before running it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

func main() {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		log.Fatal("set OPENAI_MODEL and OPENAI_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// This example delegates unknown model capabilities to the API.
	client, err := responses.New(responses.Config{UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		log.Fatal(err)
	}
	request, err := client.Prepare(ctx, llm.Input{
		Model:    model,
		Stream:   true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("Say hello in one sentence."))},
	})
	if err != nil {
		log.Fatal(err)
	}
	var result llm.Result
	for event, err := range llm.Stream(ctx, client, request) {
		if event.Delta != nil && event.Delta.Kind == llm.DeltaText {
			fmt.Print(event.Delta.Text)
		}
		if event.Result != nil {
			result = *event.Result
		}
		if err != nil {
			fmt.Println()
			log.Fatal(err)
		}
	}
	fmt.Println()
	fmt.Printf("Stop: %s; served model: %s\n", result.Stop.Kind, result.Served.Model)
}
