// Command structured_output runs one agent turn whose final answer must match
// a JSON schema, then decodes the validated value into a Go struct. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// catalog's default model.
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

	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("structured_output: create client: %w", err)
	}
	// With OutputSchema set, the turn completes only once the model submits
	// an object that validates against it.
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("OPENAI_MODEL"),
			OutputSchema: dashi.MustOutputSchema[Person](),
		},
		Client: client,
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Invent a fictional person.")),
		},
	})
	if err != nil {
		return fmt.Errorf("structured_output: run: %w", err)
	}
	// DecodeOutput explains a missing output, such as a turn that stopped
	// at a limit, in its error.
	person, err := dashi.DecodeOutput[Person](out)
	if err != nil {
		return fmt.Errorf("structured_output: decode: %w", err)
	}
	// Source says whether the model submitted the object as a tool call or
	// as its final text; either way it was validated.
	fmt.Printf("Stop: %s; output from: %s\n", out.Stop.Kind, out.Output.Source)
	fmt.Printf("%+v\n", person)
	return nil
}
