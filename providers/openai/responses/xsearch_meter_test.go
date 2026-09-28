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

// Posts and profiles are billed separately, so one counter alone does not
// account for X search items: the known count stays and the items remain
// unpriced. Built from the captured response with counters removed.
func TestXSearchItemWithOneCounterIsStillUnpriced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keep   string
		counts map[string]int
	}{
		{
			name:   "posts only",
			keep:   "x_posts_fetched",
			counts: map[string]int{llm.MeterXSearchPost: 3},
		},
		{
			name:   "users only",
			keep:   "x_users_fetched",
			counts: map[string]int{llm.MeterXSearchProfile: 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response map[string]any
			if err := json.Unmarshal(withoutXAIBilling(t), &response); err != nil {
				t.Fatal(err)
			}
			usage := response["usage"].(map[string]any)
			for _, count := range tc.counts {
				usage["server_side_tool_usage_details"] = map[string]any{
					tc.keep: count,
				}
			}
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			wire := meterCodec("grok", []string{"web_search", "x_search"})
			result, err := wire.decode(raw, llm.ModelRef{})
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
			maps.Copy(want, tc.counts)
			if result.Usage == nil || !maps.Equal(result.Usage.Requests, want) {
				t.Fatalf("usage = %+v, want requests %v", result.Usage, want)
			}
			entries := catalog.Entries("grok")
			if len(entries) == 0 || entries[0].Pricing == nil {
				t.Fatal("catalog has no priced Grok entry")
			}
			cost, ok := entries[0].Pricing.Estimate(result.Usage)
			if !ok || !cost.Partial() ||
				cost.UnpricedRequests[llm.UnrecognizedMeter("custom_tool_call")] != 1 {
				t.Fatalf("one counter left the estimate complete: %+v", cost)
			}
		})
	}
}

// A reported total includes tool charges, so it accounts for X search items
// even when no fetched-item counter is present.
func TestXSearchItemCoveredByReportedCost(t *testing.T) {
	var response map[string]any
	if err := json.Unmarshal(withoutXAIBilling(t), &response); err != nil {
		t.Fatal(err)
	}
	response["usage"].(map[string]any)["cost_in_usd_ticks"] = 166303000
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	wire := meterCodec("grok", []string{"web_search", "x_search"})
	result, err := wire.decode(raw, llm.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cost == nil {
		t.Fatal("reported cost was dropped")
	}
	if _, ok := result.Usage.Requests[llm.UnrecognizedMeter("custom_tool_call")]; ok {
		t.Fatalf("X search item marked unpriced despite a reported cost: %v",
			result.Usage.Requests)
	}
}

// Both captured Grok responses list every fetched-item counter, zeros
// included; the rule does not depend on it, but it is the evidence that a
// missing counter is unusual.
func TestCapturedXAIDetailsListZeroCounters(t *testing.T) {
	var response struct {
		Usage struct {
			Details map[string]json.RawMessage `json:"server_side_tool_usage_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(
		readFixture(t, "grok-web-x-search.json"),
		&response,
	); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"x_posts_fetched", "x_users_fetched"} {
		if _, ok := response.Usage.Details[key]; !ok {
			t.Fatalf("captured details lack %s", key)
		}
	}
	if string(response.Usage.Details["x_users_fetched"]) != "0" {
		t.Fatalf("x_users_fetched = %s, want an explicit 0",
			response.Usage.Details["x_users_fetched"])
	}
}
