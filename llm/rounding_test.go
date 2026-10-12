package llm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/money"
)

// subNanoSheet prices every bucket at 2500 nanos per million tokens, so each
// token costs 0.0025 nanos and rounding is visible.
func subNanoSheet() Pricing {
	p := pricedSheet()
	rate := amt("0.0000025")
	p.Base = Rates{
		Input:      rate,
		Output:     rate,
		CacheRead:  rate,
		CacheWrite: rate,
	}
	return p
}

func TestPricingEstimateRoundsHalfEvenOnce(t *testing.T) {
	p := subNanoSheet()
	for _, tc := range []struct {
		input int
		want  int64
	}{
		{200, 0},  // 0.5 nanos: tie to even
		{600, 2},  // 1.5 nanos: tie to even
		{1000, 2}, // 2.5 nanos: tie to even
		{1400, 4}, // 3.5 nanos: tie to even
		{1401, 4}, // 3.5025 nanos: above the tie
		{1399, 3}, // 3.4975 nanos: below the tie
	} {
		cost, ok := p.Estimate(&Usage{InputTokens: tc.input})
		if !ok || cost.Total.Nanos() != tc.want {
			t.Fatalf(
				"%d tokens = %s, %v; want %d nanos",
				tc.input,
				cost.Total,
				ok,
				tc.want,
			)
		}
	}
	// Each half nano rounds to zero, but the total rounds the exact sum.
	cost, ok := p.Estimate(&Usage{InputTokens: 200, OutputTokens: 200})
	if !ok ||
		cost.Input != 0 ||
		cost.Output != 0 ||
		cost.Total.Nanos() != 1 {
		t.Fatalf("total was not rounded once: %+v, %v", cost, ok)
	}
}

func TestMaxCostAtRiskRoundsUp(t *testing.T) {
	p := subNanoSheet()
	snapshot := riskRequest(t, 100, nil).Snapshot()
	snapshot.EstimatedInputTokens = 1035
	request, err := NewRequest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cost, ok := WorstCaseCost(request, p)
	if !ok {
		t.Fatal("priced request had no cost bound")
	}
	// 1035 input tokens are 2.5875 nanos and 100 output tokens 0.25 nanos;
	// each rounds up, and the total rounds up the exact 2.8375 once.
	if EstimatedInputTokens(request) != 1035 {
		t.Fatalf("input tokens=%d", EstimatedInputTokens(request))
	}
	if cost.Input.Nanos() != 3 || cost.Output.Nanos() != 1 ||
		cost.Total.Nanos() != 3 {
		t.Fatalf("risk was not rounded up: %+v", cost)
	}
}

func TestMaxCostAtRiskRejectsOverflow(t *testing.T) {
	p := pricedSheet()
	p.Base.Output = money.AmountFromNanos(math.MaxInt64)
	request := riskRequest(t, 2_000_000, nil)
	if _, ok := WorstCaseCost(request, p); ok {
		t.Fatal("an overflowing risk bound was treated as known")
	}
	if fit := FitOutputTokens(request, p, amt("1")); fit != 0 {
		t.Fatalf("fit=%d under an overflowing bound", fit)
	}
}

func TestCostAndPricingJSONAcceptBareNumbers(t *testing.T) {
	var cost Cost
	raw := `{"input":0.1,"output":0.2,"cache_read":0,"cache_write":0,` +
		`"total":0.30000000000000004,"currency":"USD",` +
		`"source":"list_price_estimate"}`
	if err := json.Unmarshal([]byte(raw), &cost); err != nil {
		t.Fatal(err)
	}
	if cost.Total != amt("0.3") || cost.Input != amt("0.1") {
		t.Fatalf("numeric cost = %+v", cost)
	}
	encoded, err := json.Marshal(cost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"total":"0.3"`) {
		t.Fatalf("encoded %s", encoded)
	}

	var sheet Pricing
	raw = `{"currency":"USD","updated_at":"2026-09-27","base":` +
		`{"input":1.25,"output":"10","cache_read":0.125,` +
		`"cache_write":"1.5625"}}`
	if err := json.Unmarshal([]byte(raw), &sheet); err != nil {
		t.Fatal(err)
	}
	if sheet.Base.Input != amt("1.25") ||
		sheet.Base.CacheWrite != amt("1.5625") {
		t.Fatalf("mixed sheet = %+v", sheet.Base)
	}
}

func TestFitOutputTokensAtTheExactBoundary(t *testing.T) {
	p := pricedSheet()
	p.Base.Output = amt("3.3")
	request := riskRequest(t, 1000, nil)
	cost, ok := WorstCaseCost(request, p)
	if !ok {
		t.Fatal("priced request had no cost bound")
	}
	if fit := FitOutputTokens(request, p, cost.Total); fit != 1000 {
		t.Fatalf("an exact fit returned %d", fit)
	}
	short := cost.Total - 1
	fit := FitOutputTokens(request, p, short)
	if fit >= 1000 || fit <= 0 {
		t.Fatalf("one nano short returned %d", fit)
	}
	for _, tokens := range []int{fit, fit + 1} {
		smaller := riskRequest(t, tokens, nil)
		risk, ok := WorstCaseCost(smaller, p)
		if !ok {
			t.Fatal("smaller request had no cost bound")
		}
		if fits := risk.Total <= short; fits != (tokens == fit) {
			t.Fatalf(
				"%d tokens risk %s against %s",
				tokens,
				risk.Total,
				short,
			)
		}
	}
}
