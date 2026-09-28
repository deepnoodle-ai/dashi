package catalog

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
)

// TestEveryCatalogPriceIsExactDecimalText keeps price data out of binary
// floats: each rate is a JSON string that parses exactly and is already in
// canonical form, so the file reads the same as the amount Kernel uses.
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
		})
	}
}
