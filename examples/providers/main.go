// Command providers sends the same one-shot prompt to the provider chosen by
// -provider, then prints the answer, the model that served it, and its cost.
// Only client construction differs between providers; the call is the same.
//
// Set the chosen provider's key: ANTHROPIC_API_KEY, OPENAI_API_KEY,
// OPENROUTER_API_KEY, XAI_API_KEY, MISTRAL_API_KEY, DEEPINFRA_API_KEY, or
// META_API_KEY. Ollama needs no key but must be running locally. Use -model
// to override the provider's small default model.
//
// Gemini is left out: providers/google/gemini is a separate Go module, so
// importing it would add a dependency to this one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"reflect"
	"slices"
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

// providers lists the supported names. Where the catalog knows a provider,
// catalog.Small picks its inexpensive model; otherwise smallModels does.
var providers = []string{
	"anthropic", "deepinfra", "grok", "meta", "mistral", "ollama", "openai",
	"openrouter",
}

// smallModels holds an inexpensive model for providers whose catalog names
// none. Model IDs are the provider's native names; Dashi does not translate
// them.
var smallModels = map[string]string{
	"openrouter": "openai/gpt-5-nano",
	"ollama":     "llama3.2",
	"deepinfra":  "meta-llama/Llama-3.3-70B-Instruct-Turbo",
	"meta":       "muse-spark-1.3",
}

func main() {
	log.SetFlags(0)
	provider := flag.String("provider", "anthropic", "provider: "+
		strings.Join(providers, ", "))
	model := flag.String("model", "", "model ID (default: per provider)")
	prompt := flag.String(
		"prompt",
		"Explain quantum computing in one sentence.",
		"prompt to send",
	)
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
	result, err := llm.Generate(
		ctx,
		client,
		llm.Input{
			Model: model,
			Messages: []llm.Message{
				llm.UserMessage(llm.Text(prompt)),
			},
		},
		nil,
	)
	if err != nil {
		return err
	}
	fmt.Println(result.Message.Text())
	fmt.Printf("\nServed by: %s %s\n", result.Served.Provider, result.Served.Model)
	fmt.Println("Cost:", cost(client, model, result))
	return nil
}

// newClient is the only provider-specific code. Each constructor reads its
// key from the environment when Config leaves it empty; a missing key is
// reported by the first call. Returning client only when err is nil keeps a
// nil client pointer out of the interface.
func newClient(provider string) (llm.Client, error) {
	var client llm.Client
	var err error
	switch provider {
	case "anthropic":
		client, err = messages.New(messages.Config{})
	case "openai":
		client, err = responses.New(responses.Config{})
	case "openrouter":
		client, err = openrouter.New(openrouter.Config{})
	case "ollama":
		client, err = ollama.New(ollama.Config{})
	case "grok":
		client, err = grok.New(grok.Config{})
	case "mistral":
		client, err = mistral.New(mistral.Config{})
	case "deepinfra":
		client, err = deepinfra.New(deepinfra.Config{})
	case "meta":
		client, err = meta.New(meta.Config{})
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}

// cost prefers a provider-reported charge, such as OpenRouter's. Otherwise
// it estimates from the catalog's list prices, but only when that price sheet
// provably applies to this call; any doubt prints "unknown" with the reason.
// docs/guides/model-calls.md explains the checks.
func cost(client llm.Client, model string, result llm.Result) string {
	if c := result.Cost; c != nil {
		return formatCost(*c)
	}
	info, ok := client.ModelInfo(model)
	if !ok || info.Pricing == nil {
		return "unknown (no price sheet for this model)"
	}
	if !catalogPriceQualified(client, model, result, info) {
		return "unknown (served model or catalog price sheet not qualified)"
	}
	if !defaultServiceTier(client.Provider(), result.ServiceTier) {
		return "unknown (service tier not covered by price sheet)"
	}
	if result.Usage == nil {
		return "unknown (usage not reported)"
	}
	c, ok := info.Pricing.Estimate(result.Usage)
	if !ok {
		return "unknown (usage cannot support a price estimate)"
	}
	return formatCost(c)
}

// catalogPriceQualified reports whether the client's price sheet for model is
// the unmodified built-in catalog sheet for this API and endpoint, and whether
// the provider served that model or a snapshot the catalog entry covers.
func catalogPriceQualified(
	client llm.Client,
	model string,
	result llm.Result,
	info llm.ModelInfo,
) bool {
	evidence := info.Evidence
	if evidence.Owner != catalog.Owner || evidence.Model != model ||
		evidence.API != client.API() ||
		result.Requested.Provider != client.Provider() ||
		result.Requested.Model != model ||
		result.Served.Provider != client.Provider() {
		return false
	}
	documented, ok := catalog.Lookup(
		evidence.API,
		evidence.Endpoint,
		model,
	)
	if !ok || documented.Pricing == nil ||
		!reflect.DeepEqual(evidence, documented.Evidence) ||
		!reflect.DeepEqual(info.Pricing, documented.Pricing) {
		return false
	}
	entry, ok := catalog.Find(client.Provider(), model)
	return ok && reflect.DeepEqual(entry.Pricing, documented.Pricing) &&
		entry.Serves(result.Served.Model)
}

// defaultServiceTier reports whether tier is the provider's default tier,
// the only one catalog price sheets describe. An unfamiliar value does not
// qualify.
func defaultServiceTier(provider, tier string) bool {
	switch provider {
	case "anthropic":
		return tier == "" || tier == "standard"
	case "openai", "grok":
		return tier == "" || tier == "default"
	case "meta", "mistral":
		return tier == ""
	default:
		return false
	}
}

func formatCost(c llm.Cost) string {
	if c.Partial() {
		meters := slices.Sorted(maps.Keys(c.UnpricedRequests))
		parts := make([]string, 0, len(meters))
		for _, meter := range meters {
			parts = append(parts, fmt.Sprintf(
				"%s=%d",
				meter,
				c.UnpricedRequests[meter],
			))
		}
		return fmt.Sprintf(
			"at least %s %s (%s; unpriced hosted requests: %s)",
			c.Total,
			c.Currency,
			c.Source,
			strings.Join(parts, ", "),
		)
	}
	return fmt.Sprintf("%s %s (%s)", c.Total, c.Currency, c.Source)
}
