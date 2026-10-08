package catalog_test

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// miniResult is a gpt-5-mini call that the public OpenAI client served as
// asked.
func miniResult() llm.Result {
	return llm.Result{
		Requested: llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Served:    llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Usage:     &llm.Usage{InputTokens: 1000},
	}
}

func openAIClient(t *testing.T) llm.Client {
	t.Helper()
	client, err := responses.New(responses.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// costText returns the cost as text, or "unknown: " and the reason.
func costText(client llm.Client, result llm.Result) string {
	cost, err := catalog.Cost(client, result)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return cost.String()
}

func TestCostEstimatesServedCatalogModel(t *testing.T) {
	openai := openAIClient(t)
	if got := costText(openai, miniResult()); got !=
		"0.00025 USD (list_price_estimate)" {
		t.Fatalf("matching model cost = %q", got)
	}
	tier := miniResult()
	tier.ServiceTier = "default"
	if got := costText(openai, tier); got != "0.00025 USD (list_price_estimate)" {
		t.Fatalf("default tier cost = %q", got)
	}

	anthropic, err := messages.New(messages.Config{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := llm.Result{
		Requested: llm.ModelRef{Provider: "anthropic", Model: "claude-haiku-4-5"},
		Served: llm.ModelRef{
			Provider: "anthropic",
			Model:    "claude-haiku-4-5-20251001",
		},
		ServiceTier: "standard",
		Usage:       &llm.Usage{InputTokens: 1000},
	}
	if got := costText(anthropic, snapshot); got != "0.001 USD (list_price_estimate)" {
		t.Fatalf("documented snapshot cost = %q", got)
	}
}

func TestCostRefusesUnqualifiedEstimate(t *testing.T) {
	openai := openAIClient(t)
	for _, tc := range []struct {
		name   string
		change func(*llm.Result)
		want   string
	}{
		{"empty served model", func(r *llm.Result) { r.Served = llm.ModelRef{} }, "not the catalog's"},
		{"different model", func(r *llm.Result) { r.Served.Model = "gpt-5-nano" }, "not the catalog's"},
		{"different provider", func(r *llm.Result) { r.Served.Provider = "grok" }, "not the catalog's"},
		{"other requested provider", func(r *llm.Result) { r.Requested.Provider = "grok" }, "not the catalog's"},
		{"unknown model", func(r *llm.Result) { r.Requested.Model = "unknown-model" }, "no price for model"},
		{"priority tier", func(r *llm.Result) { r.ServiceTier = "priority" }, `service tier "priority"`},
		{"missing usage", func(r *llm.Result) { r.Usage = nil }, "no usage"},
		{"insufficient usage", func(r *llm.Result) {
			r.Usage = &llm.Usage{InputTokens: 1000, TotalTokenCountMismatch: true}
		}, "not enough to price"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := miniResult()
			tc.change(&r)
			_, err := catalog.Cost(openai, r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Cost error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCostRefusesChangedCatalogEntry(t *testing.T) {
	original, ok := catalog.Lookup(
		responses.API,
		"https://api.openai.com/v1/responses",
		"gpt-5-mini",
	)
	if !ok {
		t.Fatal("catalog model is missing")
	}
	for _, tc := range []struct {
		name     string
		endpoint string
		change   func(*llm.ModelInfo)
	}{
		{name: "changed price", change: func(m *llm.ModelInfo) {
			m.Pricing.Base.Input = money.MustParseAmount("99")
		}},
		{name: "changed source", change: func(m *llm.ModelInfo) {
			m.Evidence.Source = "https://example.test/changed"
		}},
		{name: "changed owner", change: func(m *llm.ModelInfo) {
			m.Evidence.Owner = "application"
		}},
		{
			name:     "changed endpoint",
			endpoint: "https://proxy.example.test/v1/responses",
			change: func(m *llm.ModelInfo) {
				m.Evidence.Endpoint = "https://proxy.example.test/v1/responses"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			custom := original.Clone()
			tc.change(&custom)
			client, err := responses.New(responses.Config{
				Provider: "openai",
				Endpoint: tc.endpoint,
				Models:   map[string]llm.ModelInfo{"gpt-5-mini": custom},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.Cost(client, miniResult()); err == nil {
				t.Fatal("Cost estimated from a changed catalog entry")
			}
		})
	}
}

func TestCostKeepsZeroPartialAndReportedCosts(t *testing.T) {
	openai := openAIClient(t)
	zero := miniResult()
	zero.Usage = &llm.Usage{}
	if got := costText(openai, zero); got != "0 USD (list_price_estimate)" {
		t.Fatalf("known zero usage cost = %q", got)
	}
	partial := miniResult()
	partial.Usage.Requests = map[string]int{llm.MeterImageGeneration: 1}
	if got := costText(openai, partial); got !=
		"at least 0.00025 USD (list_price_estimate; "+
			"unpriced provider tool requests: image_generation=1)" {
		t.Fatalf("partial estimate cost = %q", got)
	}
	// A provider-reported cost wins, even when the catalog could not
	// estimate one.
	reported := llm.Result{Cost: &llm.Cost{
		Total:    money.MustParseAmount("0.04"),
		Currency: "USD",
		Source:   llm.CostSourceProviderReported,
	}}
	if got := costText(openai, reported); got != "0.04 USD (provider_reported)" {
		t.Fatalf("reported cost = %q", got)
	}
}

func TestCostServiceTiersPerProvider(t *testing.T) {
	for _, tc := range []struct {
		provider string
		tier     string
		want     bool
	}{
		{"openai", "", true},
		{"openai", "default", true},
		{"openai", "priority", false},
		{"anthropic", "", true},
		{"anthropic", "standard", true},
		{"anthropic", "fast", false},
		{"grok", "default", true},
		{"grok", "priority", false},
		{"meta", "", true},
		{"meta", "auto", false},
		{"mistral", "", true},
		{"mistral", "unknown", false},
	} {
		if got := catalog.PricedServiceTier(tc.provider, tc.tier); got != tc.want {
			t.Errorf("PricedServiceTier(%q, %q) = %t, want %t",
				tc.provider, tc.tier, got, tc.want)
		}
	}
}
