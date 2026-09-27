package llm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func pricedSheet() Pricing {
	return Pricing{
		Currency:  "USD",
		UpdatedAt: "2026-09-27",
		Reference: "fixture",
		Base: Rates{
			Input:      1,
			Output:     2,
			CacheRead:  0.1,
			CacheWrite: 1.25,
		},
	}
}

func TestPricingEstimateUsesLiteralRates(t *testing.T) {
	p := pricedSheet()
	u := &Usage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	cost, ok := p.Estimate(u)
	if !ok {
		t.Fatal("complete usage could not be priced")
	}
	if cost.Input != 1 || cost.Output != 2 || cost.CacheRead != 0.1 ||
		cost.CacheWrite != 1.25 || cost.Total != 4.35 ||
		cost.Currency != "USD" || cost.Source != CostSourceListPriceEstimate {
		t.Fatalf("unexpected estimate: %+v", cost)
	}

	p.Base.CacheRead = 0
	cost, ok = p.Estimate(&Usage{CacheReadInputTokens: 1_000_000})
	if !ok || cost.CacheRead != 0 || cost.Total != 0 {
		t.Fatalf("literal free cache reads were not priced: %+v, %v", cost, ok)
	}
	if _, ok := p.Estimate(nil); ok {
		t.Fatal("missing usage became a known zero cost")
	}
}

func TestPricingEstimateTierAndModality(t *testing.T) {
	p := pricedSheet()
	p.Tiers = []PriceTier{{
		MinInputTokens: 100,
		Rates: Rates{
			Input:      3,
			Output:     6,
			CacheRead:  0.3,
			CacheWrite: 3.75,
		},
	}}
	p.Modality = map[string]Rates{
		"audio": {
			Input:      10,
			Output:     20,
			CacheRead:  1,
			CacheWrite: 12,
		},
	}
	u := &Usage{
		InputTokens:              120,
		OutputTokens:             10,
		CacheReadInputTokens:     10,
		CacheCreationInputTokens: 10,
		ModalityTokens: map[string]ModalityTokenUsage{
			"audio": {
				InputTokens:              40,
				OutputTokens:             4,
				CacheReadInputTokens:     4,
				CacheCreationInputTokens: 4,
			},
			"text": {
				InputTokens:              80,
				OutputTokens:             6,
				CacheReadInputTokens:     6,
				CacheCreationInputTokens: 6,
			},
		},
	}
	cost, ok := p.Estimate(u)
	if !ok {
		t.Fatal("tiered multimodal usage could not be priced")
	}
	// Audio keeps its modality rates; text uses the threshold-selected rates.
	want := (80*3 + 40*10 + 6*6 + 4*20 +
		6*0.3 + 4*1 + 6*3.75 + 4*12) / 1_000_000.0
	if math.Abs(cost.Total-want) > 1e-12 {
		t.Fatalf("tier and modality total = %v, want %v", cost.Total, want)
	}
	u.InputTokens = 79
	u.ModalityTokens["text"] = ModalityTokenUsage{
		InputTokens:              39,
		OutputTokens:             6,
		CacheReadInputTokens:     6,
		CacheCreationInputTokens: 6,
	}
	u.CacheReadInputTokens = 0
	u.CacheCreationInputTokens = 0
	delete(u.ModalityTokens, "text")
	delete(u.ModalityTokens, "audio")
	u.OutputTokens = 0
	cost, ok = p.Estimate(u)
	if !ok || cost.Input != float64(79)/1_000_000 {
		t.Fatalf("below-threshold base rate = %+v, %v", cost, ok)
	}
}

func TestPricingEstimateRejectsMissingBillingDetail(t *testing.T) {
	p := pricedSheet()
	p.Modality = map[string]Rates{
		"audio": {
			Input:      4,
			Output:     8,
			CacheRead:  0.4,
			CacheWrite: 5,
		},
	}
	for _, tc := range []struct {
		name  string
		usage Usage
	}{
		{
			name: "cache writes unreported",
			usage: Usage{
				InputTokens:                         100,
				CacheCreationInputTokensUnavailable: true,
			},
		},
		{
			name: "input modality incomplete",
			usage: Usage{
				InputTokens:                         100,
				InputModalityTokenDetailsIncomplete: true,
			},
		},
		{
			name: "output modality incomplete",
			usage: Usage{
				OutputTokens:                         100,
				OutputModalityTokenDetailsIncomplete: true,
			},
		},
		{
			name: "cache read modality incomplete",
			usage: Usage{
				CacheReadInputTokens:                    100,
				CacheReadModalityTokenDetailsIncomplete: true,
			},
		},
		{
			name: "cache write modality incomplete",
			usage: Usage{
				CacheCreationInputTokens: 100,
				ModalityTokens: map[string]ModalityTokenUsage{
					"audio": {CacheCreationInputTokens: 40},
				},
			},
		},
		{
			name:  "provider total mismatch",
			usage: Usage{TotalTokenCountMismatch: true},
		},
		{
			name: "overlapping modality buckets",
			usage: Usage{
				InputTokens: 10,
				ModalityTokens: map[string]ModalityTokenUsage{
					"audio": {InputTokens: 11},
				},
			},
		},
		{
			name:  "negative tokens",
			usage: Usage{InputTokens: -1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := p.Estimate(&tc.usage); ok {
				t.Fatal("incomplete usage produced a cost estimate")
			}
		})
	}
}

func TestPricingValidationAndStoredJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Pricing)
	}{
		{"missing currency", func(p *Pricing) { p.Currency = "" }},
		{"invalid date", func(p *Pricing) { p.UpdatedAt = "2026-02-30" }},
		{"negative rate", func(p *Pricing) { p.Base.Input = -1 }},
		{"infinite rate", func(p *Pricing) { p.Base.Output = math.Inf(1) }},
		{"unordered tiers", func(p *Pricing) {
			p.Tiers = []PriceTier{
				{MinInputTokens: 10},
				{MinInputTokens: 10},
			}
		}},
		{"empty modality", func(p *Pricing) {
			p.Modality = map[string]Rates{"": {}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pricedSheet()
			tc.edit(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("invalid price sheet was accepted")
			}
		})
	}
	for _, raw := range []string{
		`{"currency":"USD","updated_at":"2026-09-27","input_price_per_1m_tokens":2}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":null}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":1}}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":0,"output":0,"cache_read":0,"cache_write":0},"input_price_per_1m_tokens":2}`,
	} {
		var p Pricing
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Fatalf("legacy or incomplete sheet was accepted: %s", raw)
		}
	}
	p := pricedSheet()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Pricing
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("valid price sheet did not round trip: %v", err)
	}
	if !strings.Contains(string(raw), `"reference":"fixture"`) ||
		restored.Base.CacheWrite != p.Base.CacheWrite {
		t.Fatal("rate provenance or literal rates were lost")
	}
}

func TestPricingCloneOwnsTiersAndModalities(t *testing.T) {
	p := pricedSheet()
	p.Tiers = []PriceTier{{MinInputTokens: 100, Rates: p.Base}}
	p.Modality = map[string]Rates{"audio": p.Base}
	copy := p.Clone()
	copy.Tiers[0].Rates.Input = 9
	copy.Modality["audio"] = Rates{Input: 9}
	if p.Tiers[0].Rates.Input != 1 || p.Modality["audio"].Input != 1 {
		t.Fatal("pricing clone shared mutable rate data")
	}
}
