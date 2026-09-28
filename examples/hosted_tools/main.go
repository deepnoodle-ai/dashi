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
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Answer)

	// The provider may split hosted work across several model calls, so
	// these read every call in the turn, not only the last message.
	fmt.Println("\nHosted tool calls:")
	for _, use := range outcome.Turn.HostedUses() {
		fmt.Printf("  %s %s\n", use.Name, use.Input)
	}
	fmt.Println("\nSources:")
	seen := map[string]bool{}
	for _, citation := range outcome.Turn.Citations() {
		if !seen[citation.URL] {
			seen[citation.URL] = true
			fmt.Printf("  %s <%s>\n", citation.Title, citation.URL)
		}
	}

	spend := outcome.Spend
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
