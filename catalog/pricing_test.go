package catalog

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// TestEveryCatalogPriceIsExactDecimalText keeps price data out of binary
// floats: each rate is a JSON string that parses exactly and is already in
// canonical form, so the file reads the same as the amount Dashi uses.
func TestEveryCatalogPriceIsExactDecimalText(t *testing.T) {
	type rawSheet struct {
		Base     map[string]json.RawMessage                   `json:"base"`
		Tiers    []struct{ Rates map[string]json.RawMessage } `json:"tiers"`
		Modality map[string]map[string]json.RawMessage        `json:"modality"`
		Requests map[string]json.RawMessage                   `json:"requests"`
	}
	names, err := files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	check := func(where string, rates map[string]json.RawMessage) {
		for name, raw := range rates {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				t.Fatalf(
					"%s %s is %s, not a decimal string",
					where,
					name,
					raw,
				)
			}
			amount, err := money.ParseAmount(text)
			if err != nil {
				t.Fatalf("%s %s: %v", where, name, err)
			}
			if amount.String() != text {
				t.Fatalf(
					"%s %s = %q, canonical %q",
					where,
					name,
					text,
					amount,
				)
			}
			checked++
		}
	}
	for _, f := range names {
		data, err := files.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			Models []struct {
				Model   string    `json:"model"`
				Pricing *rawSheet `json:"pricing"`
			} `json:"models"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		for _, m := range file.Models {
			if m.Pricing == nil {
				continue
			}
			where := f.Name() + " " + m.Model
			check(where+" base", m.Pricing.Base)
			for _, tier := range m.Pricing.Tiers {
				check(where+" tier", tier.Rates)
			}
			for modality, rates := range m.Pricing.Modality {
				check(where+" "+modality, rates)
			}
			check(where+" requests", m.Pricing.Requests)
		}
	}
	if checked == 0 {
		t.Fatal("no catalog prices were checked")
	}
}

// TestCatalogEstimatesAreExact compares estimates with totals computed by
// hand from the published per-million rates.
func TestCatalogEstimatesAreExact(t *testing.T) {
	for _, tc := range []struct {
		provider string
		model    string
		usage    llm.Usage
		total    string
		unpriced map[string]int
	}{
		{
			// 1234×10 + 567×50 + 8901×0.25 + 234×12.5 = 45840.25
			provider: "anthropic",
			model:    "claude-fable-5-1",
			usage: llm.Usage{
				InputTokens:              1234,
				OutputTokens:             567,
				CacheReadInputTokens:     8901,
				CacheCreationInputTokens: 234,
			},
			total: "0.04584025",
		},
		{
			// 1000×10 + 2000×12.5 + 1000×20 (one-hour writes at 2x) = 55000
			provider: "anthropic",
			model:    "claude-fable-5-1",
			usage: llm.Usage{
				InputTokens:                1000,
				CacheCreationInputTokens:   3000,
				CacheCreation1hInputTokens: 1000,
			},
			total: "0.055",
		},
		{
			// 1000×2 + 500×10 + 10000×0.2 + 2000×2.5 + 1000×4 = 18000,
			// plus two web searches at 0.01 each.
			provider: "anthropic",
			model:    "claude-sonnet-5-5",
			usage: llm.Usage{
				InputTokens:                1000,
				OutputTokens:               500,
				CacheReadInputTokens:       10000,
				CacheCreationInputTokens:   3000,
				CacheCreation1hInputTokens: 1000,
				Requests:                   map[string]int{"web_search": 2},
			},
			total: "0.038",
		},
		{
			// 1000×2 + 500×10 + 10000×0.1 + 1000×2.5 = 10500, plus one web
			// search at 0.01. Cache reads are 5% of input on this model.
			provider: "openai",
			model:    "gpt-6.1-sol",
			usage: llm.Usage{
				InputTokens:              1000,
				OutputTokens:             500,
				CacheReadInputTokens:     10000,
				CacheCreationInputTokens: 1000,
				Requests:                 map[string]int{"web_search": 1},
			},
			total: "0.0205",
		},
		{
			// 1001×0.75 + 333×3.75 + 7×0.075 = 2000.025
			provider: "google",
			model:    "gemini-3.8-flash",
			usage: llm.Usage{
				InputTokens:          1001,
				OutputTokens:         333,
				CacheReadInputTokens: 7,
			},
			total: "0.002000025",
		},
		{
			// 100000×12.5 + 20000×75 + 50000×1.25 + 1000×15.625 = 2828125
			provider: "openai",
			model:    "gpt-5.6-cyber",
			usage: llm.Usage{
				InputTokens:              100_000,
				OutputTokens:             20_000,
				CacheReadInputTokens:     50_000,
				CacheCreationInputTokens: 1_000,
			},
			total: "2.828125",
		},
		{
			// 3×0.05 + 1×0.4 + 1×0.005 = 0.555. A catalog rate has at most
			// six decimals, so every token costs a whole number of nanos
			// and the smallest charges stay exact.
			provider: "openai",
			model:    "gpt-5-nano",
			usage: llm.Usage{
				InputTokens:          3,
				OutputTokens:         1,
				CacheReadInputTokens: 1,
			},
			total: "0.000000555",
		},
		{
			// 1000×1 per million = 0.001, plus 3 searches at 0.01 and two
			// free fetches. Code execution has no rate, so the total is a
			// lower bound and names it.
			provider: "anthropic",
			model:    "claude-haiku-4-5",
			usage: llm.Usage{
				InputTokens: 1000,
				Requests: map[string]int{
					llm.MeterWebSearch:     3,
					llm.MeterWebFetch:      2,
					llm.MeterCodeExecution: 1,
				},
			},
			total:    "0.031",
			unpriced: map[string]int{llm.MeterCodeExecution: 1},
		},
		{
			// 2 grounded prompts at 0.035 and 5 search queries at no charge,
			// with no tokens.
			provider: "google",
			model:    "gemini-2.5-flash",
			usage: llm.Usage{
				Requests: map[string]int{
					llm.MeterGroundedPrompt: 2,
					llm.MeterWebSearch:      5,
				},
			},
			total: "0.07",
		},
	} {
		t.Run(tc.model, func(t *testing.T) {
			e, ok := Find(tc.provider, tc.model)
			if !ok || e.Pricing == nil {
				t.Fatalf(
					"%s is missing or unpriced",
					tc.model,
				)
			}
			cost, ok := e.Pricing.Estimate(&tc.usage)
			if !ok {
				t.Fatal("estimate was unavailable")
			}
			if want := money.MustParseAmount(tc.total); cost.Total != want {
				t.Fatalf(
					"total = %s, want %s",
					cost.Total,
					want,
				)
			}
			if !maps.Equal(cost.UnpricedRequests, tc.unpriced) {
				t.Fatalf(
					"unpriced = %v, want %v",
					cost.UnpricedRequests,
					tc.unpriced,
				)
			}
		})
	}
}
