// Command structured_output runs one agent turn whose final reply must match
// a JSON schema, then decodes the validated value into a Go struct. Set
// OPENAI_API_KEY before running it; OPENAI_MODEL optionally overrides the
// default model.
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

// Person is the shape of the final reply. The JSON schema the model sees is
// generated from this struct.
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
		return err
	}
	runner := dashi.Runner{Client: client}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		// With OutputSchema set, the turn completes only once the model
		// submits an object that validates against it.
		Agent: dashi.Agent{
			Model:        os.Getenv("OPENAI_MODEL"),
			OutputSchema: dashi.MustOutputSchema[Person](),
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Invent a fictional person.")),
		},
	})
	if err != nil {
		return err
	}
	// DecodeOutput returns an error if the turn ended without a valid
	// object, for example because it hit a limit.
	person, err := dashi.DecodeOutput[Person](out)
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n", out.Stop.Kind)
	fmt.Printf("%+v\n", person)
	return nil
}
