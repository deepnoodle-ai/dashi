package gemini

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A Gemini result names no service tier, so catalog.Cost must accept the
// empty tier to estimate a public-endpoint call.
func TestCatalogCostEstimatesGeminiCall(t *testing.T) {
	client, err := New(Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	model := "gemini-3.8-flash"
	cost, err := catalog.Cost(client, llm.Result{
		Requested: llm.ModelRef{Provider: "google", Model: model},
		Served:    llm.ModelRef{Provider: "google", Model: model},
		Usage:     &llm.Usage{InputTokens: 1000},
	})
	if err != nil {
		t.Fatalf("catalog.Cost: %v", err)
	}
	if cost.Source != llm.CostSourceListPriceEstimate || cost.Total.Nanos() == 0 {
		t.Fatalf("cost = %s, want a nonzero list-price estimate", cost)
	}
}
