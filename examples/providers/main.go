// Command providers sends one prompt to the provider chosen by -provider and
// prints the reply, the model that served it, and what the call cost. Notice
// that only newClient differs between providers; the call is the same.
// catalog.Cost reports a cost when the provider reports one or the built-in
// catalog's list price applies, and otherwise says why it cannot.
//
// Set the chosen provider's key: ANTHROPIC_API_KEY, OPENAI_API_KEY,
// OPENROUTER_API_KEY, XAI_API_KEY, MISTRAL_API_KEY, DEEPINFRA_API_KEY, or
// META_API_KEY. Ollama needs no key but must be running locally. Use -model
// to pick a model; Gemini lives in its own Go module, so it is left out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/deepinfra"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/mistral"
	"github.com/deepnoodle-ai/dashi/providers/ollama"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/providers/openrouter"
)

// providers lists the supported names. catalog.Small names an inexpensive
// model for the providers the catalog knows; smallModels covers the rest.
var providers = []string{
	"anthropic", "deepinfra", "grok", "meta", "mistral", "ollama", "openai",
	"openrouter",
}

// smallModels holds an inexpensive model for providers the catalog does not
// know. Model IDs are the provider's own names.
var smallModels = map[string]string{
	"openrouter": "openai/gpt-5-nano",
	"ollama":     "llama3.2",
	"deepinfra":  "meta-llama/Llama-3.3-70B-Instruct-Turbo",
	"meta":       "muse-spark-1.3",
}

func main() {
	log.SetFlags(0)
	provider := flag.String("provider", "anthropic",
		"provider: "+strings.Join(providers, ", "))
	model := flag.String("model", "", "model ID (default: per provider)")
	prompt := flag.String("prompt",
		"Explain quantum computing in one sentence.", "prompt to send")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *provider, *model, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, provider, model, prompt string) error {
	if model == "" {
		var ok bool
		if model, ok = catalog.Small(provider); !ok {
			model = smallModels[provider]
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	client, err := newClient(provider)
	if err != nil {
		return err
	}
	// The nil sink means no streamed text; Generate returns the whole reply.
	result, err := llm.Generate(ctx, client, llm.Input{
		Model:    model,
		Messages: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	}, nil)
	if err != nil {
		return err
	}
	fmt.Println(result.Message.Text())
	fmt.Printf("\nServed by: %s %s\n", result.Served.Provider, result.Served.Model)
	if cost, err := catalog.Cost(client, result); err != nil {
		fmt.Println("Cost: unknown,", err)
	} else {
		fmt.Println("Cost:", cost)
	}
	return nil
}

// newClient is the only provider-specific code. Each constructor reads its
// API key from the environment when Config leaves it empty.
func newClient(provider string) (llm.Client, error) {
	switch provider {
	case "anthropic":
		return messages.New(messages.Config{})
	case "openai":
		return responses.New(responses.Config{})
	case "openrouter":
		return openrouter.New(openrouter.Config{})
	case "ollama":
		return ollama.New(ollama.Config{})
	case "grok":
		return grok.New(grok.Config{})
	case "mistral":
		return mistral.New(mistral.Config{})
	case "deepinfra":
		return deepinfra.New(deepinfra.Config{})
	case "meta":
		return meta.New(meta.Config{})
	}
	return nil, fmt.Errorf("unknown provider %q", provider)
}
