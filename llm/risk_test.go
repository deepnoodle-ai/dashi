package llm

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func riskRequest(
	t *testing.T,
	maxTokens int,
	tools []tool.Definition,
) Request {
	t.Helper()
	input := Input{
		Model:    "fixture",
		Settings: Settings{MaxTokens: maxTokens},
		Tools:    tools,
	}
	request, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    "fixture",
			Scope:    "fixture",
		},
		Input:            input,
		Effective:        input.Settings,
		Body:             []byte(`{"ok":true}`),
		CapabilityPolicy: CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestMaxCostAtRiskBoundsInputAndOutput(t *testing.T) {
	p := pricedSheet()
	p.Base = Rates{
		Input:      amt("1"),
		Output:     amt("2"),
		CacheRead:  amt("3"),
		CacheWrite: amt("4"),
	}
	request := riskRequest(t, 100, nil)
	cost, ok := MaxCostAtRisk(request, p)
	if !ok {
		t.Fatal("priced request had no cost bound")
	}
	if EstimatedInputTokens(request) != len(request.Body())+1024 {
		t.Fatal("input bound did not include body and fixed margin")
	}
	// Input tokens × $4/MTok is 4 µ$ per token, exact in nanos.
	wantInput := money.AmountFromNanos(int64(EstimatedInputTokens(request)) * 4000)
	if cost.Input != wantInput {
		t.Fatalf("input cost=%s", cost.Input)
	}
	if cost.Output != amt("0.0002") {
		t.Fatalf("output cost=%s", cost.Output)
	}
	fit := FitOutputTokens(request, p, cost.Input+amt("0.0001"))
	if fit != 50 {
		t.Fatalf("fit=%d", fit)
	}
}

func TestMaxCostAtRiskRejectsUnboundedHostedCalls(t *testing.T) {
	p := pricedSheet()
	p.Requests = map[string]money.Amount{"web_search": amt("0.01")}
	request := riskRequest(
		t,
		100,
		[]tool.Definition{
			{
				Name: "web_search",
				Kind: tool.KindProviderHosted,
				Hosted: &tool.HostedSpec{
					Provider: "test",
					API:      "test.chat",
					Type:     "web_search",
				},
			},
		},
	)
	if _, ok := MaxCostAtRisk(request, p); ok {
		t.Fatal("unbounded hosted calls must not pass a hard cap")
	}
}

func TestPricingEstimateIncludesHostedRequests(t *testing.T) {
	p := pricedSheet()
	p.Requests = map[string]money.Amount{"web_search": amt("0.01")}
	cost, ok := p.Estimate(&Usage{
		InputTokens: 100,
		Requests:    map[string]int{"web_search": 2},
	})
	if !ok {
		t.Fatal("hosted request was not priced")
	}
	if cost.Hosted != amt("0.02") {
		t.Fatalf("hosted cost=%s", cost.Hosted)
	}
	if cost.Total != amt("0.0201") {
		t.Fatalf("total cost=%s", cost.Total)
	}
}

func TestPricingEstimateMarksUnpricedRequests(t *testing.T) {
	p := pricedSheet()
	p.Requests = map[string]money.Amount{"web_search": amt("0.01")}
	cost, ok := p.Estimate(&Usage{
		InputTokens: 100,
		Requests: map[string]int{
			"web_search":     1,
			"code_execution": 2,
			"file_search":    0,
		},
	})
	if !ok {
		t.Fatal("token cost is known, so the estimate must be available")
	}
	if !cost.Partial() ||
		!reflect.DeepEqual(cost.UnpricedRequests, map[string]int{"code_execution": 2}) {
		t.Fatalf("unpriced=%v", cost.UnpricedRequests)
	}
	if cost.Hosted != amt("0.01") || cost.Total != amt("0.0101") {
		t.Fatalf("hosted=%s total=%s", cost.Hosted, cost.Total)
	}
	complete, ok := p.Estimate(&Usage{
		InputTokens: 100,
		Requests:    map[string]int{"web_search": 1},
	})
	if !ok || complete.Partial() {
		t.Fatalf("fully priced cost marked partial: %+v", complete)
	}
	clone := cost.Clone()
	clone.UnpricedRequests["code_execution"] = 9
	if cost.UnpricedRequests["code_execution"] != 2 {
		t.Fatal("clone shares unpriced counts")
	}
	raw, err := json.Marshal(cost)
	if err != nil {
		t.Fatal(err)
	}
	var restored Cost
	if err := json.Unmarshal(raw, &restored); err != nil ||
		!reflect.DeepEqual(restored, cost) {
		t.Fatalf("partial cost did not round trip: %+v, %v", restored, err)
	}
	if err := (Cost{
		Currency:         "USD",
		Source:           CostSourceListPriceEstimate,
		UnpricedRequests: map[string]int{"web_search": 0},
	}).Validate(); err == nil {
		t.Fatal("zero unpriced count must be rejected")
	}
}

func TestPricingRejectsReservedMeterKeys(t *testing.T) {
	for _, key := range []string{
		MeterUnreported,
		UnrecognizedMeter("mystery_call"),
	} {
		p := pricedSheet()
		p.Requests = map[string]money.Amount{key: amt("0.01")}
		if err := p.Validate(); err == nil {
			t.Fatalf("sheet priced reserved key %q", key)
		}
	}
	if !IsReservedMeter(UnrecognizedMeter("x")) || IsReservedMeter(MeterWebSearch) {
		t.Fatal("reserved meter classification is wrong")
	}
}
