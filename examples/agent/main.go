// Command agent runs a recorded turn. Set OPENAI_API_KEY and OPENAI_MODEL.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/deepnoodle-ai/kernel"
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
	client, err := responses.New(responses.Config{})
	if err != nil {
		log.Fatal(err)
	}
	definition := kernel.Definition{
		ID:       "example",
		Revision: "1",
		Provider: client.Provider(),
		API:      client.API(),
		Model:    model,
		Limits:   kernel.DefaultLimits(),
	}
	recorder := kernel.NewMemoryRecorder()
	outcome, err := (kernel.Engine{}).Apply(
		ctx,
		kernel.Turn{ID: "example-turn"},
		definition,
		kernel.Start{
			ID: "example-start",
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Say hello in one sentence.")),
			},
		},
		kernel.Bindings{Client: client, Recorder: recorder},
	)
	if err != nil {
		log.Fatal(err)
	}
	if outcome.Persistence != kernel.PersistenceAcknowledged {
		log.Fatalf("recording: %s: %v", outcome.Persistence, outcome.WriteError)
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
}
