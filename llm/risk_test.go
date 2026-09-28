package llm

import (
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/tool"
)

func riskRequest(t *testing.T, maxTokens int, tools []tool.Definition) Request {
	t.Helper()
	input := Input{Model: "fixture", Settings: Settings{MaxTokens: maxTokens},
		Tools: tools}
	request, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{Provider: "test", API: "test.chat",
			Model: "fixture", Scope: "fixture"},
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
	p.Base = Rates{Input: 1, Output: 2,
		CacheRead: 3, CacheWrite: 4}
	request := riskRequest(t, 100, nil)
	cost, ok := MaxCostAtRisk(request, p)
	if !ok || EstimatedInputTokens(request) != len(request.Body())+1024 ||
		cost.Input != float64(EstimatedInputTokens(request))*4/1_000_000 ||
		cost.Output != 0.0002 {
		t.Fatalf("cost=%+v ok=%t", cost, ok)
	}
	fit := FitOutputTokens(request, p, cost.Input+0.0001)
	if fit != 50 {
		t.Fatalf("fit=%d", fit)
	}
}

func TestMaxCostAtRiskRejectsUnboundedHostedCalls(t *testing.T) {
	p := pricedSheet()
	p.Requests = map[string]float64{"web_search": 0.01}
	request := riskRequest(t, 100, []tool.Definition{{
		Name: "web_search",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{Provider: "test", API: "test.chat",
			Type: "web_search"},
	}})
	if _, ok := MaxCostAtRisk(request, p); ok {
		t.Fatal("unbounded hosted calls must not pass a hard cap")
	}
}

func TestPricingEstimateIncludesHostedRequests(t *testing.T) {
	p := pricedSheet()
	p.Requests = map[string]float64{"web_search": 0.01}
	cost, ok := p.Estimate(&Usage{
		InputTokens: 100,
		Requests:    map[string]int{"web_search": 2},
	})
	if !ok || cost.Hosted != 0.02 || cost.Total < 0.02 {
		t.Fatalf("cost=%+v ok=%t", cost, ok)
	}
	if _, ok := p.Estimate(&Usage{Requests: map[string]int{"unknown": 1}}); ok {
		t.Fatal("missing hosted request price must not count as free")
	}
}
