package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/decide"
)

const decisionModel = "jev-1.13.0"

var errBudget = errors.New("evaluation cost reserve exhausted")

// spendLimit owns the run's reservations, including failed calls. A later
// result never releases a reservation for another paid request.
type spendLimit struct {
	mu                      sync.Mutex
	cap, reserved, observed money.Amount
	requests, unknown       int
}

type spending struct {
	cap, reserved, observed money.Amount
	requests, unknown       int
}

func newSpendLimit(limit string) (*spendLimit, error) {
	cap, err := money.ParseAmount(limit)
	if err != nil || cap <= 0 {
		return nil, errors.New("-live and -preflight require a positive -max-cost in USD")
	}
	return &spendLimit{cap: cap}, nil
}

func (b *spendLimit) reserve(cost money.Amount) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cost < 0 || cost > b.cap-b.reserved {
		return errBudget
	}
	b.reserved += cost
	b.requests++
	return nil
}

func (b *spendLimit) observe(cost money.Amount, known bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !known {
		b.unknown++
		return
	}
	b.observed += cost
}

func (b *spendLimit) snapshot() spending {
	b.mu.Lock()
	defer b.mu.Unlock()
	return spending{b.cap, b.reserved, b.observed, b.requests, b.unknown}
}

// limitedClient composes the framework's request cost bound with one
// run-wide ledger. Prepare still performs no network access.
type limitedClient struct {
	llm.Client
	limit     *spendLimit
	preflight bool
	reply     string
}

func (c *limitedClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	snapshot := call.Request.Snapshot()
	if !textInput(snapshot.Input) {
		return llm.Result{}, errors.New("evaluation cost reserve requires text-only input")
	}
	if snapshot.ModelInfo == nil || snapshot.ModelInfo.Pricing == nil || snapshot.ModelInfo.Pricing.Currency != "USD" {
		return llm.Result{}, errors.New("evaluation request has no USD price sheet")
	}
	price := *snapshot.ModelInfo.Pricing
	cost, ok := llm.WorstCaseCost(call.Request, price)
	if !ok {
		return llm.Result{}, errors.New("evaluation request has no conservative cost bound")
	}
	if err := c.limit.reserve(cost.Total); err != nil {
		return llm.Result{}, err
	}
	if c.preflight {
		result := dashitest.Reply(c.reply)
		result.Requested = llm.ModelRef{Provider: snapshot.Origin.Provider, Model: snapshot.Origin.Model}
		result.Served = result.Requested
		result.Usage = &llm.Usage{}
		return result, nil
	}
	result, err := c.Client.Send(ctx, call)
	actual, known := price.Estimate(result.Usage)
	c.limit.observe(actual.Total, known && err == nil)
	return result, err
}

// Remote media can consume more tokens than its encoded URL. This eval's
// byte bound therefore admits text only, including text tool results.
func textInput(input llm.Input) bool {
	textBlocks := func(blocks []content.Block) bool {
		for _, block := range blocks {
			if block.Text == nil {
				return false
			}
		}
		return true
	}
	for _, message := range input.Messages {
		for _, part := range message.Content {
			if part.Block != nil && part.Block.Text == nil {
				return false
			}
			if part.ToolResult != nil && !textBlocks(part.ToolResult.Output.Content) {
				return false
			}
			if part.ProviderToolResult != nil && !textBlocks(part.ProviderToolResult.Output) {
				return false
			}
			if part.ProviderToolUse != nil && !textBlocks(part.ProviderToolUse.Output) {
				return false
			}
		}
	}
	return true
}

// limitedDecide reserves one native request. The SDK is configured with no
// retries, so one invocation is at most one paid dispatch.
type limitedDecide struct {
	decide.Transport
	limit *spendLimit
}

func (t *limitedDecide) SystemOne(ctx context.Context, req *decide.Request) (*decide.Response, error) {
	if req.Model != decisionModel {
		return nil, fmt.Errorf("evaluation Decide model must be %s", decisionModel)
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, errors.New("cannot encode evaluation Decide request")
	}
	// A token consumes at least a byte. Framing has the same allowance as
	// llm.EstimatedInputTokens. Jev's price is 42 nanos per input token.
	bound := money.Amount(len(encoded)+1024) * 42
	if err := t.limit.reserve(bound); err != nil {
		return nil, err
	}
	resp, err := t.Transport.SystemOne(ctx, req)
	if resp == nil {
		t.limit.observe(0, false)
	} else {
		t.limit.observe(money.Amount(resp.Usage.InputTokens)*42, err == nil)
	}
	return resp, err
}

// singleDispatchHTTP disables reuse and HTTP/2 connection retries. The SDK
// and model runners add no retries, so every reservation covers one dispatch.
func singleDispatchHTTP() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
