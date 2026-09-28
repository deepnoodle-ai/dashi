package llm

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/money"
)

func amt(s string) money.Amount {
	return money.MustParseAmount(s)
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
	if cost.Input != amt("1") || cost.Output != amt("2") ||
		cost.CacheRead != amt("0.1") ||
		cost.CacheWrite != amt("1.25") ||
		cost.Total != amt("4.35") ||
		cost.Currency != "USD" ||
		cost.Source != CostSourceListPriceEstimate {
		t.Fatalf("unexpected estimate: %+v", cost)
	}

	p.Base.CacheRead = 0
	cost, ok = p.Estimate(&Usage{CacheReadInputTokens: 1_000_000})
	if !ok || cost.CacheRead != 0 || cost.Total != 0 {
		t.Fatalf(
			"literal free cache reads were not priced: %+v, %v",
			cost,
			ok,
		)
	}
	if _, ok := p.Estimate(nil); ok {
		t.Fatal("missing usage became a known zero cost")
	}
}

func TestPricingEstimateTierAndModality(t *testing.T) {
	p := pricedSheet()
	p.Tiers = []PriceTier{
		{
			MinInputTokens: 100,
			Rates: Rates{
				Input:      amt("3"),
				Output:     amt("6"),
				CacheRead:  amt("0.3"),
				CacheWrite: amt("3.75"),
			},
		},
	}
	p.Modality = map[string]Rates{
		"audio": {
			Input:      amt("10"),
			Output:     amt("20"),
			CacheRead:  amt("1"),
			CacheWrite: amt("12"),
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
	// 240+400+36+80 + 1.8+4+22.5+48 = 832.3 dollar-tokens per MTok.
	want := amt("0.0008323")
	if cost.Total != want {
		t.Fatalf(
			"tier and modality total = %v, want %v",
			cost.Total,
			want,
		)
	}
	boundary := &Usage{
		InputTokens: 99,
		ModalityTokens: map[string]ModalityTokenUsage{
			"text": {InputTokens: 99},
		},
	}
	cost, ok = p.Estimate(boundary)
	if !ok || cost.Input != amt("0.000099") {
		t.Fatalf("below-threshold base rate = %+v, %v", cost, ok)
	}
	boundary.InputTokens = 100
	boundary.ModalityTokens["text"] = ModalityTokenUsage{InputTokens: 100}
	cost, ok = p.Estimate(boundary)
	if !ok || cost.Input != amt("0.0003") {
		t.Fatalf("inclusive tier rate = %+v, %v", cost, ok)
	}
}

func TestPricingEstimateRejectsMissingBillingDetail(t *testing.T) {
	p := pricedSheet()
	p.Modality = map[string]Rates{
		"audio": {
			Input:      amt("4"),
			Output:     amt("8"),
			CacheRead:  amt("0.4"),
			CacheWrite: amt("5"),
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
					"audio": {
						CacheCreationInputTokens: 40,
					},
				},
			},
		},
		{
			name:  "input has no modality attribution",
			usage: Usage{InputTokens: 100},
		},
		{
			name:  "output has no modality attribution",
			usage: Usage{OutputTokens: 100},
		},
		{
			name:  "cache reads have no modality attribution",
			usage: Usage{CacheReadInputTokens: 100},
		},
		{
			name:  "cache writes have no modality attribution",
			usage: Usage{CacheCreationInputTokens: 100},
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
			name: "unrecognized modality capitalization",
			usage: Usage{
				InputTokens: 10,
				ModalityTokens: map[string]ModalityTokenUsage{
					"Audio": {InputTokens: 10},
				},
			},
		},
		{
			name:  "negative tokens",
			usage: Usage{InputTokens: -1},
		},
		{
			name: "input count overflow",
			usage: Usage{
				InputTokens:          math.MaxInt,
				CacheReadInputTokens: 1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := p.Estimate(&tc.usage); ok {
				t.Fatal("incomplete usage produced a cost estimate")
			}
		})
	}
}

func TestPricingEstimateNeedsDetailOnlyWhereRatesDiffer(t *testing.T) {
	p := pricedSheet()
	rates := p.Base
	rates.Output = amt("8")
	p.Modality = map[string]Rates{"audio": rates}
	cost, ok := p.Estimate(&Usage{InputTokens: 100})
	if !ok || cost.Input != amt("0.0001") {
		t.Fatalf(
			"unaffected input needed modality detail: %+v, %v",
			cost,
			ok,
		)
	}
}

func TestPricingEstimateRejectsArithmeticOverflow(t *testing.T) {
	p := pricedSheet()
	p.Base.Input = money.AmountFromNanos(math.MaxInt64)
	if _, ok := p.Estimate(&Usage{InputTokens: 10_000_000}); ok {
		t.Fatal("unrepresentable cost was returned as an estimate")
	}
}

func TestPricingValidateRejectsInvalidSheet(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Pricing)
	}{
		{"missing currency", func(p *Pricing) { p.Currency = "" }},
		{
			"invalid date",
			func(p *Pricing) { p.UpdatedAt = "2026-02-30" },
		},
		{"negative rate", func(p *Pricing) { p.Base.Input = -1 }},
		{
			"negative hosted price",
			func(p *Pricing) {
				p.Requests = map[string]money.Amount{
					"web_search": -1,
				}
			},
		},
		{
			name: "unordered tiers",
			edit: func(p *Pricing) {
				p.Tiers = []PriceTier{
					{MinInputTokens: 10},
					{MinInputTokens: 10},
				}
			},
		},
		{
			name: "empty modality",
			edit: func(p *Pricing) {
				p.Modality = map[string]Rates{"": {}}
			},
		},
		{
			name: "uppercase modality",
			edit: func(p *Pricing) {
				p.Modality = map[string]Rates{"Audio": {}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pricedSheet()
			tc.edit(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("invalid price sheet was accepted")
			}
		})
	}
}

func TestPricingStoredJSONRequiresCompleteRates(t *testing.T) {
	for _, raw := range []string{
		`{"currency":"USD","updated_at":"2026-09-27","input_price_per_1m_tokens":2}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":null}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":1}}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":0,"output":0,"cache_read":0,"cache_write":0},"tiers":[{"min_input_tokens":100}]}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":0,"output":0,"cache_read":0,"cache_write":0},"tiers":[{"min_input_tokens":100,"rates":null}]}`,
		`{"currency":"USD","updated_at":"2026-09-27","base":{"input":0,"output":0,"cache_read":0,"cache_write":0},"input_price_per_1m_tokens":2}`,
	} {
		var p Pricing
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Fatalf(
				"legacy or incomplete sheet was accepted: %s",
				raw,
			)
		}
	}
}

func TestPricingStoredJSONRoundTrips(t *testing.T) {
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
	copy.Tiers[0].Rates.Input = amt("9")
	copy.Modality["audio"] = Rates{Input: amt("9")}
	if p.Tiers[0].Rates.Input != amt("1") ||
		p.Modality["audio"].Input != amt("1") {
		t.Fatal("pricing clone shared mutable rate data")
	}
}

func TestCostStoredJSONRequiresCurrentAttemptShape(t *testing.T) {
	for _, raw := range []string{
		`{"total":0.5,"currency":"USD","source":"provider_reported","model":"old"}`,
		`{"total":0.5,"currency":"USD","source":"mixed"}`,
		`{"total":-0.5,"currency":"USD","source":"provider_estimate"}`,
		`{"total":0.5,"currency":"","source":"provider_estimate"}`,
		`{"currency":"USD","source":"provider_reported"}`,
		`{"total":null,"currency":"USD","source":"provider_reported"}`,
	} {
		var cost Cost
		if err := json.Unmarshal([]byte(raw), &cost); err == nil {
			t.Fatalf(
				"invalid or legacy cost was accepted: %s",
				raw,
			)
		}
	}
	knownZero := Cost{
		Total:    0,
		Currency: "USD",
		Source:   CostSourceProviderReported,
	}
	raw, err := json.Marshal(knownZero)
	if err != nil {
		t.Fatal(err)
	}
	var restored Cost
	if err := json.Unmarshal(raw, &restored); err != nil ||
		!reflect.DeepEqual(restored, knownZero) {
		t.Fatalf(
			"known zero cost did not round trip: %+v, %v",
			restored,
			err,
		)
	}
}

func pricedSheet() Pricing {
	return Pricing{
		Currency:  "USD",
		UpdatedAt: "2026-09-27",
		Reference: "fixture",
		Base: Rates{
			Input:      amt("1"),
			Output:     amt("2"),
			CacheRead:  amt("0.1"),
			CacheWrite: amt("1.25"),
		},
	}
}
