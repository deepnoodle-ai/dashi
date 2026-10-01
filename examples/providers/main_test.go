package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

func TestCostQualifiesServedModel(t *testing.T) {
	openai, err := responses.New(responses.Config{})
	if err != nil {
		t.Fatal(err)
	}
	base := llm.Result{
		Requested: llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Served:    llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Usage:     &llm.Usage{InputTokens: 1000},
	}
	if got := cost(openai, "gpt-5-mini", base); got !=
		"0.00025 USD (list_price_estimate)" {
		t.Fatalf("matching model cost = %q", got)
	}
	priority := base
	priority.ServiceTier = "priority"
	if got := cost(openai, "gpt-5-mini", priority); got !=
		"unknown (service tier not covered by price sheet)" {
		t.Fatalf("priority tier cost = %q", got)
	}

	for _, tc := range []struct {
		name   string
		change func(*llm.Result)
	}{
		{"empty served model", func(r *llm.Result) { r.Served = llm.ModelRef{} }},
		{"different model", func(r *llm.Result) { r.Served.Model = "gpt-5-nano" }},
		{"different provider", func(r *llm.Result) { r.Served.Provider = "grok" }},
		{"different request", func(r *llm.Result) {
			r.Requested.Model = "gpt-5-nano"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.change(&r)
			if got := cost(openai, "gpt-5-mini", r); !strings.HasPrefix(
				got,
				"unknown (served model or catalog price sheet not qualified)",
			) {
				t.Fatalf("unqualified cost = %q", got)
			}
		})
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
	if got := cost(anthropic, "claude-haiku-4-5", snapshot); got !=
		"0.001 USD (list_price_estimate)" {
		t.Fatalf("documented snapshot cost = %q", got)
	}
}

func TestCostRejectsChangedCatalogEvidence(t *testing.T) {
	original, ok := catalog.Lookup(
		responses.API,
		"https://api.openai.com/v1/responses",
		"gpt-5-mini",
	)
	if !ok {
		t.Fatal("catalog model is missing")
	}
	result := llm.Result{
		Requested: llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Served:    llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Usage:     &llm.Usage{InputTokens: 1000},
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
			if got := cost(client, "gpt-5-mini", result); got !=
				"unknown (served model or catalog price sheet not qualified)" {
				t.Fatalf("changed catalog claim cost = %q", got)
			}
		})
	}
}

func TestCostKeepsUnknownAndPartialVisible(t *testing.T) {
	client, err := responses.New(responses.Config{})
	if err != nil {
		t.Fatal(err)
	}
	base := llm.Result{
		Requested: llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Served:    llm.ModelRef{Provider: "openai", Model: "gpt-5-mini"},
		Usage:     &llm.Usage{InputTokens: 1000},
	}
	if got := cost(client, "unknown-model", base); got !=
		"unknown (no price sheet for this model)" {
		t.Fatalf("missing sheet cost = %q", got)
	}
	missingUsage := base
	missingUsage.Usage = nil
	if got := cost(client, "gpt-5-mini", missingUsage); got !=
		"unknown (usage not reported)" {
		t.Fatalf("missing usage cost = %q", got)
	}
	knownZero := base
	knownZero.Usage = &llm.Usage{}
	if got := cost(client, "gpt-5-mini", knownZero); got !=
		"0 USD (list_price_estimate)" {
		t.Fatalf("known zero usage cost = %q", got)
	}
	insufficient := base
	insufficient.Usage = &llm.Usage{
		InputTokens:             1000,
		TotalTokenCountMismatch: true,
	}
	if got := cost(client, "gpt-5-mini", insufficient); got !=
		"unknown (usage cannot support a price estimate)" {
		t.Fatalf("insufficient usage cost = %q", got)
	}
	partial := base
	partial.Usage = &llm.Usage{
		InputTokens: 1000,
		Requests:    map[string]int{llm.MeterImageGeneration: 1},
	}
	if got := cost(client, "gpt-5-mini", partial); got !=
		"at least 0.00025 USD (list_price_estimate; "+
			"unpriced provider tool requests: image_generation=1)" {
		t.Fatalf("partial estimate cost = %q", got)
	}
	partial.Cost = &llm.Cost{
		Total:    money.MustParseAmount("0.04"),
		Currency: "USD",
		Source:   llm.CostSourceProviderReported,
		UnpricedRequests: map[string]int{
			"z_meter": 2,
			"a_meter": 1,
		},
	}
	partial.Served = llm.ModelRef{}
	if got := cost(client, "gpt-5-mini", partial); got !=
		"at least 0.04 USD (provider_reported; "+
			"unpriced provider tool requests: a_meter=1, z_meter=2)" {
		t.Fatalf("partial reported cost = %q", got)
	}
	partial.Cost.UnpricedRequests = nil
	if got := cost(client, "gpt-5-mini", partial); got !=
		"0.04 USD (provider_reported)" {
		t.Fatalf("reported cost = %q", got)
	}
	partial.Cost.Source = llm.CostSourceProviderEstimate
	if got := cost(client, "gpt-5-mini", partial); got !=
		"0.04 USD (provider_estimate)" {
		t.Fatalf("provider estimate cost = %q", got)
	}
}

func TestDefaultServiceTier(t *testing.T) {
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
		{"other", "", false},
	} {
		if got := defaultServiceTier(tc.provider, tc.tier); got != tc.want {
			t.Errorf("defaultServiceTier(%q, %q) = %t, want %t",
				tc.provider,
				tc.tier,
				got,
				tc.want,
			)
		}
	}
}
