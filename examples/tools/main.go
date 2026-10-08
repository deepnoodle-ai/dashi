// Command tools runs one agent turn in which the model calls typed Go
// functions as tools, then prints the reply and what the turn cost. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the default model.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

// weatherArgs holds get_weather's arguments. Fields without omitempty are
// required, and each description tag tells the model what the field means.
type weatherArgs struct {
	City string `json:"city" description:"City name, such as Tokyo"`
}

type convertArgs struct {
	Celsius float64 `json:"celsius" description:"Temperature in Celsius"`
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

	tools := newTools()
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	// The runner calls a tool whenever the model asks for it.
	runner := dashi.Runner{Client: client, Tools: tools}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Answer weather questions using the tools.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("What's the weather in Tokyo, in Fahrenheit?")),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)

	// Spend totals every model call in the turn.
	fmt.Printf(
		"Model calls: %d; tokens: %d in, %d out\n",
		out.ModelCalls,
		out.Spend.Usage.TotalInputTokens(),
		out.Spend.Usage.OutputTokens,
	)
	fmt.Printf("Cost: %s\n", out.Spend)
	return nil
}

// newTools builds the tools. tool.MustFunc describes each tool's arguments
// to the model from its Go type, and checks the model's arguments before the
// function runs.
func newTools() tool.List {
	weather := tool.MustFunc(
		"get_weather",
		"Get the current weather for a city, in Celsius.",
		func(_ context.Context, _ tool.Call, in weatherArgs) (tool.Outcome, error) {
			fmt.Printf("-> get_weather(%q)\n", in.City)
			if strings.TrimSpace(in.City) == "" {
				// A plain error is returned to the model as a failed call.
				return tool.Outcome{}, errors.New("city is required")
			}
			return tool.Succeeded("18°C, partly cloudy"), nil
		},
	)
	convert := tool.MustFunc(
		"celsius_to_fahrenheit",
		"Convert a temperature from Celsius to Fahrenheit.",
		func(_ context.Context, _ tool.Call, in convertArgs) (tool.Outcome, error) {
			fmt.Printf("-> celsius_to_fahrenheit(%g)\n", in.Celsius)
			f := in.Celsius*9/5 + 32
			return tool.Succeeded(fmt.Sprintf("%.1f°F", f)), nil
		},
	)
	return tool.List{weather, convert}
}
