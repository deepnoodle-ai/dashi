package responses

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
)

// withoutXAIBilling returns the captured Grok response with its usage
// counters and reported cost removed, keeping token usage and output items.
// The captured response has one web_search_call and one X search
// custom_tool_call (x_keyword_search).
func withoutXAIBilling(t *testing.T) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(
		readFixture(t, "grok-web-x-search.json"),
		&response,
	); err != nil {
		t.Fatal(err)
	}
	usage := response["usage"].(map[string]any)
	for _, key := range []string{
		"server_side_tool_usage_details",
		"cost_in_usd_ticks",
		"num_server_side_tools_used",
		"num_sources_used",
	} {
		delete(usage, key)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestXSearchItemWithoutCountersIsUnpriced(t *testing.T) {
	wire := meterCodec("grok", []string{"web_search", "x_search"})
	result, err := wire.decode(withoutXAIBilling(t), llm.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cost != nil {
		t.Fatalf("no cost was reported, got %+v", result.Cost)
	}
	want := map[string]int{
		llm.MeterWebSearch:                        1,
		llm.UnrecognizedMeter("custom_tool_call"): 1,
	}
	if result.Usage == nil || !maps.Equal(result.Usage.Requests, want) {
		t.Fatalf("usage = %+v, want requests %v", result.Usage, want)
	}
	entries := catalog.Entries("grok")
	if len(entries) == 0 || entries[0].Pricing == nil {
		t.Fatal("catalog has no priced Grok entry")
	}
	cost, ok := entries[0].Pricing.Estimate(result.Usage)
	if !ok {
		t.Fatal("token cost is known, so the estimate must be available")
	}
	if !cost.Partial() ||
		cost.UnpricedRequests[llm.UnrecognizedMeter("custom_tool_call")] != 1 {
		t.Fatalf("X search item left the estimate complete: %+v", cost)
	}
}

func TestXSearchItemCoveredByCounters(t *testing.T) {
	wire := meterCodec("grok", []string{"web_search", "x_search"})
	result, err := wire.decode(
		readFixture(t, "grok-web-x-search.json"),
		llm.ModelRef{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Usage.Requests[llm.UnrecognizedMeter("custom_tool_call")]; ok {
		t.Fatalf("counted X search item twice: %v", result.Usage.Requests)
	}
	if result.Usage.Requests[llm.MeterXSearchPost] != 1 {
		t.Fatalf("requests = %v", result.Usage.Requests)
	}
}

// A custom_tool_call is a client tool call unless X search was declared.
func TestCustomToolCallWithoutXSearchIsNotHosted(t *testing.T) {
	wire := meterCodec("openai", []string{"web_search"})
	requests := wire.hostedRequests(meterFields{}, []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","call_id":"c1","name":"grammar","input":"x"}`),
	})
	if len(requests) != 0 {
		t.Fatalf("client custom tool counted as hosted: %v", requests)
	}
}
