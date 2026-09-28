// Command tools runs one agent turn in which the model calls typed Go
// function tools, then prints the answer and the turn's usage and cost. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
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

// WeatherInput is the model-facing argument schema for get_weather. Fields
// without omitempty are required, and the description tag reaches the model.
type WeatherInput struct {
	City string `json:"city" description:"City name, such as Tokyo"`
}

// ConvertInput is the argument schema for celsius_to_fahrenheit.
type ConvertInput struct {
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

	tools, err := newTools()
	if err != nil {
		return fmt.Errorf("tools: %w", err)
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("tools: create client: %w", err)
	}
	// Bound tools are allowed by default; a Policy can deny or defer calls.
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Answer weather questions using the tools.",
		},
		Client: client,
		Tools:  tools,
	}
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text(
				"What's the weather in Tokyo, in Fahrenheit?",
			)),
		},
	})
	if err != nil {
		return fmt.Errorf("tools: run start: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Answer)

	usage := outcome.Spend.Usage
	fmt.Printf(
		"Model calls: %d; tokens: %d in, %d out\n",
		outcome.ModelAttempts,
		usage.InputTokens,
		usage.OutputTokens,
	)
	for currency, amount := range outcome.Spend.Cost {
		fmt.Printf("Cost: %s %s\n", amount, currency)
	}
	return nil
}

// newTools builds the typed tools. tool.Func generates each input schema from
// its Go type and validates the model's arguments before the function runs.
//
// Runner.Tools takes a tool.Toolset so the available tools can change between
// model calls; tool.List is a fixed one.
func newTools() (tool.List, error) {
	weather, err := tool.Func(
		"get_weather",
		"Get the current weather for a city, in Celsius.",
		func(
			ctx context.Context,
			call tool.Call,
			in WeatherInput,
		) (tool.Outcome, error) {
			fmt.Printf("-> get_weather(%q)\n", in.City)
			if strings.TrimSpace(in.City) == "" {
				// A plain error is returned to the model as a failed call.
				return tool.Outcome{}, errors.New("city is required")
			}
			return tool.Succeeded("18°C, partly cloudy"), nil
		},
	)
	if err != nil {
		return nil, err
	}
	convert, err := tool.Func(
		"celsius_to_fahrenheit",
		"Convert a temperature from Celsius to Fahrenheit.",
		func(
			ctx context.Context,
			call tool.Call,
			in ConvertInput,
		) (tool.Outcome, error) {
			fmt.Printf("-> celsius_to_fahrenheit(%g)\n", in.Celsius)
			f := in.Celsius*9/5 + 32
			return tool.Succeeded(fmt.Sprintf("%.1f°F", f)), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return tool.List{weather, convert}, nil
}
