// Command provider_tools runs one agent turn in which Claude uses tools that
// Anthropic runs on its own servers: web search and a code execution sandbox.
// It prints the reply, the provider tool calls, the cited sources, and the
// turn's usage. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL
// optionally overrides the default model.
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
		// These tools have no Go code: Anthropic runs them on its servers
		// during the model call.
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
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)

	// The tool work may span several model calls, so read it from the
	// whole turn rather than the last message.
	fmt.Println("\nprovider tool calls:")
	for _, use := range out.Turn.ProviderToolUses() {
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
		"\nTokens: %d in, %d out; provider tool requests: %v\n",
		spend.Usage.TotalInputTokens(),
		spend.Usage.OutputTokens,
		spend.Usage.Requests,
	)
	// Web searches have a known price. Code execution is billed by time,
	// which responses do not report, so it is listed as unpriced.
	fmt.Printf("Cost: %s\n", spend)
	if spend.CostPartial > 0 {
		fmt.Printf("  Unpriced provider tool requests: %v\n", spend.UnpricedRequests)
	}
	return nil
}
