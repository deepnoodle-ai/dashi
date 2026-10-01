package dashi_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

// An Anthropic stream cut off after a server_tool_use block, before the
// message_delta that carries the final server_tool_use counters, must not
// look fully priced: the adapter can only infer counts from the blocks it saw.
func TestCutAnthropicHostedStreamCostIsPartial(t *testing.T) {
	fixture, err := os.ReadFile(
		"providers/anthropic/messages/testdata/websearch-max-uses-exceeded-stream.sse",
	)
	if err != nil {
		t.Fatal(err)
	}
	// Keep message_start and the first server_tool_use block through its
	// content_block_stop; drop everything after, including message_delta.
	marker := []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	end := bytes.Index(fixture, marker)
	if end < 0 {
		t.Fatal("fixture lacks the first block's stop event")
	}
	cut := fixture[:end+len(marker)]
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write(cut)
		},
	))
	t.Cleanup(server.Close)

	const provider = "claude-fixture"
	const model = "claude-haiku-4-5"
	client, err := messages.New(messages.Config{
		Provider: provider,
		APIKey:   "test",
		Endpoint: server.URL,
		Models: map[string]llm.ModelInfo{
			model: {
				Pricing: &llm.Pricing{
					Currency:  "USD",
					UpdatedAt: "2026-09-28",
					Base: llm.Rates{
						Input:      money.MustParseAmount("1"),
						Output:     money.MustParseAmount("5"),
						CacheRead:  money.MustParseAmount("0.1"),
						CacheWrite: money.MustParseAmount("1.25"),
					},
					Requests: map[string]money.Amount{
						llm.MeterWebSearch: money.MustParseAmount("0.01"),
					},
				},
				Evidence: llm.ModelEvidence{
					Model:       model,
					API:         messages.API,
					Endpoint:    server.URL,
					Owner:       "Dashi tests",
					Source:      "fixture",
					Method:      llm.EvidenceFixture,
					QualifiedAt: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	search, err := messages.NewWebSearchTool(messages.WebSearchToolOptions{
		Provider: provider,
		MaxUses:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{search},
	}
	var outcome *dashi.Outcome
	for event, err := range runner.Stream(
		t.Context(),
		dashi.Turn{ID: "cut-search"},
		dashi.Start{
			CommandID: "start",
			Agent: dashi.Agent{
				ID:       "agent",
				Revision: "r1",
				Provider: provider,
				API:      messages.API,
				Model:    model,
			},
			Input: []llm.Message{
				llm.UserMessage(llm.Text("What is the population of Tokyo?")),
			},
		},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Outcome != nil {
			outcome = event.Outcome
		}
	}
	if outcome == nil {
		t.Fatal("stream ended without an outcome")
	}
	spend := outcome.Spend
	if spend.Usage.Requests[llm.MeterWebSearch] != 1 {
		t.Fatalf("inferred search count lost: %v", spend.Usage.Requests)
	}
	if spend.CostPartial != 1 ||
		spend.UnpricedRequests[llm.MeterUnreported] != 1 {
		t.Fatalf("cut-off hosted call looks fully priced: %+v", spend)
	}
	var charge *llm.Cost
	for _, step := range outcome.Turn.Steps {
		if step.Kind == dashi.StepModelCallFinished && step.Model != nil {
			charge = step.Model.Charge
		}
	}
	if charge == nil || !charge.Partial() {
		t.Fatalf("settled charge = %+v, want partial", charge)
	}
}
