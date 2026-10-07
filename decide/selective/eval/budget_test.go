package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/decide"
)

func TestReservationsAreAtomicAndNeverReleased(t *testing.T) {
	b, err := newSpendLimit("0.90")
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if b.reserve(money.MustParseAmount("0.01")) == nil {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	b.observe(0, false)
	if admitted.Load() != 90 || b.snapshot().reserved != b.cap || !errors.Is(b.reserve(1), errBudget) {
		t.Fatalf("admitted %d, spending %+v", admitted.Load(), b.snapshot())
	}
}

type countedModel struct {
	llm.Client
	calls int
}

func (c *countedModel) Send(context.Context, llm.Call) (llm.Result, error) {
	c.calls++
	return llm.Result{}, errors.New("ambiguous model failure")
}

func TestModelGuardReservesFullOutputAndFailedCalls(t *testing.T) {
	provider, err := responses.New(responses.Config{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.Prepare(context.Background(), llm.Input{
		Model: "gpt-6-luna", Settings: llm.Settings{MaxTokens: 4096},
		Messages: []llm.Message{llm.UserMessage(llm.Text("brief prompt"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &countedModel{Client: provider}
	limit, _ := newSpendLimit("0.0001") // input fits, full output does not
	guard := &limitedClient{Client: base, limit: limit}
	if _, err := guard.Send(context.Background(), llm.Call{Request: request}); !errors.Is(err, errBudget) || base.calls != 0 {
		t.Fatalf("guard allowed dispatch: calls %d, error %v", base.calls, err)
	}
	cost, ok := llm.WorstCaseCost(request, *request.Snapshot().ModelInfo.Pricing)
	if !ok {
		t.Fatal("request is unbounded")
	}
	guard.limit = &spendLimit{cap: cost.Total}
	if _, err := guard.Send(context.Background(), llm.Call{Request: request}); err == nil {
		t.Fatal("lost failure")
	}
	if _, err := guard.Send(context.Background(), llm.Call{Request: request}); !errors.Is(err, errBudget) || base.calls != 1 {
		t.Fatalf("failed reservation was released: calls %d, error %v", base.calls, err)
	}
	if guard.limit.snapshot().unknown != 1 {
		t.Fatal("ambiguous charge was treated as known")
	}
}

func TestModelGuardRejectsUnknownPriceAndOneHourCache(t *testing.T) {
	provider, _ := messages.New(messages.Config{})
	for _, model := range []string{"unknown-model", "claude-haiku-5-5"} {
		request, err := provider.Prepare(context.Background(), llm.Input{
			Model: model, Messages: []llm.Message{llm.UserMessage(llm.Text("test"))},
			Settings: llm.Settings{MaxTokens: 64, PromptCache: llm.PromptCache{TTL: time.Hour}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if model != "unknown-model" {
			snapshot := request.Snapshot()
			snapshot.ModelInfo.Pricing.Base.CacheWrite1h = nil
			for i := range snapshot.ModelInfo.Pricing.Tiers {
				snapshot.ModelInfo.Pricing.Tiers[i].Rates.CacheWrite1h = nil
			}
			request, err = llm.NewRequest(snapshot)
			if err != nil {
				t.Fatal(err)
			}
		}
		base := &countedModel{Client: provider}
		guard := &limitedClient{Client: base, limit: &spendLimit{cap: money.MustParseAmount("0.9")}}
		if _, err := guard.Send(context.Background(), llm.Call{Request: request}); err == nil || base.calls != 0 {
			t.Fatalf("%s: unbounded request dispatched", model)
		}
	}
}

type countedDecide struct {
	decide.Transport
	calls int
}

func (t *countedDecide) SystemOne(context.Context, *decide.Request) (*decide.Response, error) {
	t.calls++
	return nil, &decide.APIError{StatusCode: 503, Message: "temporary unavailable"}
}

func TestDecideGuardHasNoPaidRetry(t *testing.T) {
	base := &countedDecide{}
	guard := &limitedDecide{Transport: base, limit: &spendLimit{cap: money.MustParseAmount("0.9")}}
	client, err := decide.NewClient(decide.WithoutEnvironment(), decide.WithTransport(guard),
		decide.WithModel(decisionModel), decide.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	req := decide.NewRequest("state")
	req.Questions["needed"] = decide.Noul("needed?")
	if _, err := client.SystemOne(context.Background(), req); err == nil {
		t.Fatal("lost failure")
	}
	if base.calls != 1 || guard.limit.snapshot().requests != 1 || guard.limit.snapshot().unknown != 1 {
		t.Fatalf("spending %+v, calls %d", guard.limit.snapshot(), base.calls)
	}
	guard.limit.cap = guard.limit.snapshot().reserved
	if _, err := client.SystemOne(context.Background(), req); err == nil || base.calls != 1 {
		t.Fatal("exhausted reserve reached transport")
	}
}

func TestPreflightNeverDispatchesAndUsesRealPrices(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "TYPESAFE_API_KEY"} {
		t.Setenv(name, "")
	}
	r, err := run(context.Background(), config{
		preflight: true, maxCost: "0.9", provider: "openai", model: "gpt-6-luna",
		sessions: 1, window: 16_000, summaryTokens: 4096, probeTokens: 512,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if r.spending.reserved <= 0 || r.spending.observed != 0 || r.spending.requests <= 9 {
		t.Fatalf("preflight spending %+v", r.spending)
	}
}
