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
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	runner := dashi.Runner{
		Client: client,
		// Hosted tools have no executor: Anthropic runs them inside the
		// model call.
		Tools: tool.List{
			search,
			messages.NewCodeExecutionTool(messages.CodeExecutionToolOptions{}),
		},
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Input: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)

	// The provider may split hosted work across several model calls, so
	// these read every call in the turn, not only the last message.
	fmt.Println("\nHosted tool calls:")
	for _, use := range out.Turn.HostedUses() {
		fmt.Printf("  %s %s\n", use.Name, use.Input)
	}
	fmt.Println("\nSources:")
	seen := map[string]bool{}
	for _, citation := range out.Turn.Citations() {
		if !seen[citation.URL] {
			seen[citation.URL] = true
			fmt.Printf("  %s <%s>\n", citation.Title, citation.URL)
		}
	}

	spend := out.Spend
	fmt.Printf(
		"\nTokens: %d in, %d out; hosted requests: %v\n",
		spend.Usage.TotalInputTokens(),
		spend.Usage.OutputTokens,
		spend.Usage.Requests,
	)
	// The catalog prices tokens and the hosted requests it has list prices
	// for, such as web searches. Anthropic bills code execution by container
	// time, which responses do not report, so it stays unpriced: the cost
	// printed is then the known part, and the rest is listed.
	fmt.Printf("Cost: %s\n", spend)
	if spend.CostPartial > 0 {
		fmt.Printf("  Unpriced hosted requests: %v\n", spend.UnpricedRequests)
	}
	return nil
}
