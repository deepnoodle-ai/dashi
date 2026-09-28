// Command structured_output runs one agent turn whose final answer must match
// a JSON schema, then decodes the validated value into a Go struct. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// catalog's default model.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/wonton/schema"
)

// Person is the shape of the turn's final answer. The schema is generated
// from it, so the struct and the contract the model sees cannot drift apart.
type Person struct {
	Name       string   `json:"name" description:"Full name"`
	Age        int      `json:"age" description:"Age in years"`
	Occupation string   `json:"occupation" description:"Current job title"`
	Hobbies    []string `json:"hobbies" description:"Two or three hobbies"`
}

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

	personSchema, err := schema.Generate(
		Person{},
		schema.GenerateOptions{DisallowAdditionalProperties: true},
	)
	if err != nil {
		return fmt.Errorf("structured_output: generate schema: %w", err)
	}
	rawSchema, err := json.Marshal(personSchema)
	if err != nil {
		return fmt.Errorf("structured_output: %w", err)
	}
	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("structured_output: create client: %w", err)
	}
	// With OutputSchema set, the turn completes only once the model submits
	// an object that validates against it.
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("OPENAI_MODEL"),
			OutputSchema: rawSchema,
		},
		Client: client,
	}
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Invent a fictional person.")),
		},
	})
	if err != nil {
		return fmt.Errorf("structured_output: run start: %w", err)
	}
	if err := outcome.RecordingErr(); err != nil {
		return fmt.Errorf("structured_output: %w", err)
	}
	if outcome.Stop.Kind != dashi.StopCompleted || outcome.Output == nil {
		return fmt.Errorf(
			"structured_output: turn stopped %s: %s",
			outcome.Stop.Kind,
			outcome.Stop.Reason,
		)
	}
	var person Person
	if err := json.Unmarshal(outcome.Output.Value, &person); err != nil {
		return fmt.Errorf("structured_output: decode output: %w", err)
	}
	fmt.Printf("Source: %s\n", outcome.Output.Source)
	fmt.Printf("%+v\n", person)
	return nil
}
