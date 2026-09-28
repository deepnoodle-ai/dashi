package kernel_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
)

// sendPlans records each admission plan and otherwise allows everything.
type sendPlans struct {
	kernel.AllowLocalPolicy
	plans []kernel.SendPlan
}

func (p *sendPlans) BeforeSend(
	ctx context.Context,
	plan kernel.SendPlan,
) (kernel.ModelAuthorization, error) {
	p.plans = append(p.plans, plan)
	return p.AllowLocalPolicy.BeforeSend(ctx, plan)
}

// A cost cap used to stop every turn whose settings left MaxTokens unset on a
// provider that does not require it, because the worst case was unbounded.
// The shared default output limit now bounds it.
func TestCostCapAdmitsDefaultSettingsOnOptionalMaxTokensProvider(t *testing.T) {
	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			sent, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{
				"id": "chatcmpl-1",
				"model": "priced",
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "hello"},
					"finish_reason": "stop"
				}],
				"usage": {
					"prompt_tokens": 10,
					"completion_tokens": 2,
					"total_tokens": 12
				}
			}`)
		},
	))
	t.Cleanup(server.Close)
	client, err := completions.New(completions.Config{
		Provider: "router",
		APIKey:   "test",
		Endpoint: server.URL,
		Models: map[string]llm.ModelInfo{
			"priced": {
				Pricing: &llm.Pricing{
					Currency:  "USD",
					UpdatedAt: "2026-09-28",
					Base: llm.Rates{
						Input:  money.MustParseAmount("1"),
						Output: money.MustParseAmount("2"),
					},
				},
				Evidence: llm.ModelEvidence{
					Model:       "priced",
					API:         completions.API,
					Endpoint:    server.URL,
					Owner:       "Kernel tests",
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
	policy := &sendPlans{}
	out, err := (kernel.Engine{}).Apply(
		t.Context(),
		kernel.Turn{ID: "capped"},
		kernel.Definition{
			ID:       "agent",
			Revision: "r1",
			Provider: "router",
			API:      completions.API,
			Model:    "priced",
			Limits:   kernel.DefaultLimits(),
		},
		kernel.Start{
			ID: "start",
			Budget: &kernel.Budget{
				MaxCost: &kernel.Money{
					Amount:   money.MustParseAmount("1"),
					Currency: "USD",
				},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
		},
		kernel.Bindings{Client: client, Policy: policy},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != kernel.StopCompleted {
		t.Fatalf("capped turn with default settings stopped %+v", out.Stop)
	}
	if len(policy.plans) != 1 || policy.plans[0].AtRisk == nil {
		t.Fatal("cost risk was not bounded before dispatch")
	}
	effective := policy.plans[0].Request.Effective()
	if effective.MaxTokens != llm.DefaultMaxTokens {
		t.Fatalf(
			"effective max tokens = %d, want %d",
			effective.MaxTokens,
			llm.DefaultMaxTokens,
		)
	}
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != float64(llm.DefaultMaxTokens) {
		t.Fatalf("dispatched max_tokens = %v", body["max_tokens"])
	}
}
