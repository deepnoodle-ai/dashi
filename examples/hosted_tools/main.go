// Command hosted_tools runs one agent turn in which Claude uses tools that
// Anthropic runs on its own servers: web search and a code execution sandbox.
// It prints the answer, the hosted tool calls, the cited sources, and the
// turn's usage. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL
// optionally overrides the catalog's default model.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

const defaultPrompt = "Use web search to find the capital of Tanzania and " +
	"its approximate population. Then use code execution to calculate 53^4. " +
	"Answer briefly."

func main() {
	log.SetFlags(0)
	prompt := flag.String("prompt", defaultPrompt, "prompt to send")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, prompt string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	search, err := messages.NewWebSearchTool(messages.WebSearchToolOptions{
		MaxUses: 3,
	})
	if err != nil {
		return fmt.Errorf("hosted_tools: %w", err)
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("hosted_tools: create client: %w", err)
	}
	// The provider runs hosted tools inside the model call, so the runner
	// has no local call to authorize.
	runner := dashi.Runner{
		Agent:  dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Client: client,
		// Hosted tools have no executor: Anthropic runs them inside the
		// model call.
		Tools: tool.List{
			search,
			messages.NewCodeExecutionTool(messages.CodeExecutionToolOptions{}),
		},
	}
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return fmt.Errorf("hosted_tools: run start: %w", err)
	}
	if err := outcome.RecordingErr(); err != nil {
		return fmt.Errorf("hosted_tools: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
	printActivity(outcome.Turn)

	view, err := dashi.Replay(outcome.Turn)
	if err != nil {
		return fmt.Errorf("hosted_tools: replay: %w", err)
	}
	spend := view.Spend
	fmt.Printf(
		"\nTokens: %d in, %d out; hosted requests: %v\n",
		spend.Usage.TotalInputTokens(),
		spend.Usage.OutputTokens,
		spend.Usage.Requests,
	)
	// The catalog prices tokens but not hosted requests, so a call that used
	// hosted tools has an unknown charge rather than an understated one.
	if spend.CostUnknown > 0 {
		fmt.Printf("Cost: unknown for %d model call(s)\n", spend.CostUnknown)
	}
	for currency, amount := range spend.Cost {
		fmt.Printf("Cost: %s %s\n", amount, currency)
	}
	return nil
}

// printActivity lists the hosted tool calls and cited sources from every
// settled model call in the turn. The provider may split hosted work across
// several calls, so the last message alone can miss some of it.
func printActivity(turn dashi.Turn) {
	var sources []string
	fmt.Println("\nHosted tool calls:")
	for _, step := range turn.Steps {
		if step.Kind != dashi.StepModelSettled || step.Model == nil ||
			step.Model.Result == nil {
			continue
		}
		for _, part := range step.Model.Result.Message.Content {
			switch {
			case part.HostedUse != nil:
				use := part.HostedUse
				fmt.Printf("  %s %s\n", use.Name, use.Input)
			case part.Block != nil && part.Block.Text != nil:
				for _, citation := range part.Block.Text.Citations {
					source := citation.Title + " <" + citation.URL + ">"
					if !slices.Contains(sources, source) {
						sources = append(sources, source)
					}
				}
			}
		}
	}
	fmt.Println("\nSources:")
	for _, source := range sources {
		fmt.Printf("  %s\n", source)
	}
}
