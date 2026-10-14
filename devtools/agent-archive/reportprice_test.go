package main

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

func amt(s string) money.Amount { return money.MustParseAmount(s) }

// testSheet is a hand-made price sheet: $4 input, $20 output, $0.40 cache
// read, $5 cache write, $8 one-hour cache write per million tokens, and
// double from 272,001 input tokens, as OpenAI's long-input tier.
func testSheet() modelPrice {
	oneHour, oneHourLong := amt("8"), amt("16")
	return modelPrice{Pricing: &llm.Pricing{
		Currency:  "USD",
		UpdatedAt: "2026-09-09",
		Base: llm.Rates{Input: amt("4"), Output: amt("20"), CacheRead: amt("0.4"), CacheWrite: amt("5"),
			CacheWrite1h: &oneHour},
		Tiers: []llm.PriceTier{{MinInputTokens: 272_001, Rates: llm.Rates{Input: amt("8"), Output: amt("40"),
			CacheRead: amt("0.8"), CacheWrite: amt("10"), CacheWrite1h: &oneHourLong}}},
		Requests: map[string]money.Amount{llm.MeterWebSearch: amt("0.01")},
	}}
}

func TestPriceCallClaudeCode(t *testing.T) {
	a, why := priceCall(testSheet(), appClaudeCode, tokenCounts{Input: 100_000, CacheRead: 100_000,
		CacheWrite: 20_000, CacheWrite1h: 10_000, Output: 100_000, Reasoning: 50_000, WebSearch: 2}, "")
	// 0.4 + 0.04 + 0.05 (5-minute writes) + 0.08 (one-hour) + 2 (reasoning
	// inside) + 0.02
	if why != "" || a.Low != amt("2.59") || a.High != a.Low || a.LowerBound || a.UnpricedCalls != 0 {
		t.Fatalf("got %+v %q, want 2.59 exactly", a, why)
	}
}

func TestPriceCallLongInputTier(t *testing.T) {
	below, _ := priceCall(testSheet(), appClaudeCode, tokenCounts{Input: 72_000, CacheRead: 200_000, Output: 1000}, "")
	above, _ := priceCall(testSheet(), appClaudeCode, tokenCounts{Input: 72_001, CacheRead: 200_000, Output: 1000}, "")
	// 72,000×4 + 200,000×0.4 + 1,000×20, per million
	if below.High != amt("0.388") {
		t.Errorf("below the tier: %s, want 0.388", below.High)
	}
	// 272,001 input tokens in all: every rate doubles.
	if above.High != amt("0.776008") {
		t.Errorf("at the tier: %s, want 0.776008", above.High)
	}
	// Summing the two rows' tokens first would price both at the tier.
	var sum amount
	sum.add(below)
	sum.add(above)
	if sum.High != amt("1.164008") {
		t.Errorf("sum %s", sum.High)
	}
}

func TestPriceCallCodexHighEstimate(t *testing.T) {
	a, _ := priceCall(testSheet(), appCodex, tokenCounts{Input: 100_000, CacheRead: 100_000, Output: 10_000}, "")
	// Low: input at $4. High: the same tokens as cache writes at $5.
	if a.Low != amt("0.64") || a.High != amt("0.74") || !a.isRange() || a.LowerBound {
		t.Fatalf("got low %s high %s, want 0.64 and 0.74", a.Low, a.High)
	}
	// Claude Code reports its cache writes, so no range.
	c, _ := priceCall(testSheet(), appClaudeCode, tokenCounts{Input: 100_000}, "")
	if c.isRange() {
		t.Fatalf("Claude Code call has a range: %+v", c)
	}
}

func TestPriceCallUnpricedAndFast(t *testing.T) {
	a, why := priceCall(modelPrice{Unpriced: unpricedNotInCatalog}, appCodex, tokenCounts{Input: 10}, "")
	if why != unpricedNotInCatalog || !a.LowerBound || a.UnpricedCalls != 1 || a.High != 0 || a.Rows != 1 {
		t.Fatalf("unpriced: %+v %q", a, why)
	}
	if formatAmount(a) != "?" {
		t.Errorf("unpriced cell %q, want ?", formatAmount(a))
	}
	f, why := priceCall(testSheet(), appClaudeCode, tokenCounts{Output: 1_000_000}, speedFast)
	if why != "" || !f.LowerBound || f.UnpricedCalls != 0 || f.High != amt("20") { // output alone is no tier
		t.Fatalf("fast: %+v %q, want 20 at standard rates as a lower bound", f, why)
	}
	if formatAmount(f) != "$20.00+" {
		t.Errorf("fast cell %q", formatAmount(f))
	}
}

func TestPriceBookLookup(t *testing.T) {
	b := priceBook{}
	for _, c := range []struct {
		app, model, why string
	}{
		{appClaudeCode, "claude-opus-5-5", ""},
		{appClaudeCode, "claude-haiku-4-5-20251001", ""}, // a dated snapshot
		{appCodex, "gpt-5.5", ""},
		{appCodex, "gpt-5-codex-mini", unpricedNoPrice},
		{appCodex, "no-such-model", unpricedNotInCatalog},
		{appCodex, "", unpricedNotInCatalog},
		{appCodex, "claude-opus-5-5", unpricedNotInCatalog}, // the app picks the provider
	} {
		p := b.lookup(c.app, c.model)
		if p.Unpriced != c.why || (c.why == "") != (p.Pricing != nil) {
			t.Errorf("%s %q: %+v, want %q", c.app, c.model, p, c.why)
		}
	}
}

func TestNotInSessionFiles(t *testing.T) {
	app := tokenCounts{Input: 100, CacheRead: 1000, CacheWrite: 200_000, Output: 50, WebSearch: 3}
	archived := tokenCounts{Input: 150, CacheRead: 400, CacheWrite: 100_000, CacheWrite1h: 50_000, Output: 50}
	left := app.minus(archived)
	want := tokenCounts{CacheRead: 600, CacheWrite: 100_000, WebSearch: 3} // input clamps at zero
	if left != want {
		t.Fatalf("left %+v, want %+v", left, want)
	}
	a, why := priceNotInSessionFiles(testSheet(), left)
	// Low: 100K writes at $5; high: at $8. Plus 600 reads and 3 searches.
	if why != "" || a.Low != amt("0.53024") || a.High != amt("0.83024") || a.LowerBound {
		t.Fatalf("got %+v %q", a, why)
	}
	u, why := priceNotInSessionFiles(modelPrice{Unpriced: unpricedNoPrice}, left)
	if why != unpricedNoPrice || !u.LowerBound || u.UnpricedCalls != 0 {
		t.Fatalf("unpriced: %+v %q", u, why)
	}
	if appUsageModel("claude-opus-5-5[1m]") != "claude-opus-5-5" {
		t.Error("[1m] not dropped")
	}
}
