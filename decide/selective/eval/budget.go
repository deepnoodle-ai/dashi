package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/decide"
)

const decisionModel = "jev-1.13.0"

var (
	errBudget        = errors.New("evaluation cost reserve exhausted")
	errUnknownCharge = errors.New("evaluation stopped after an unknown charge")
)

// spendLimit owns full request reservations. Only a verified successful result
// settles to its priced cost; failed and unknown requests retain their bounds.
type spendLimit struct {
	mu                                    sync.Mutex
	cap, reserved, fullReserved, observed money.Amount
	requests, unknown, settled            int
	cancel                                context.CancelFunc
	failures                              []dispatchFailure
}

type spending struct {
	cap, reserved, fullReserved, observed money.Amount
	requests, unknown, settled            int
	failures                              []dispatchFailure
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
	if b.unknown > 0 {
		return errUnknownCharge
	}
	if cost < 0 || cost > b.cap-b.reserved {
		return errBudget
	}
	b.reserved += cost
	b.fullReserved += cost
	b.requests++
	return nil
}

func (b *spendLimit) finish(bound, cost money.Amount, known, successful bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !known {
		b.unknown++
		if b.cancel != nil {
			b.cancel()
		}
		return
	}
	b.observed += cost
	if successful && cost >= 0 && cost <= bound {
		b.reserved -= bound - cost
		b.settled++
	}
}

func (b *spendLimit) snapshot() spending {
	b.mu.Lock()
	defer b.mu.Unlock()
	return spending{b.cap, b.reserved, b.fullReserved, b.observed, b.requests, b.unknown, b.settled, slices.Clone(b.failures)}
}

// dispatchFailure contains only public model identities and classified metadata.
// Never retain an error string, body, source case ID, or request/response text.
type dispatchFailure struct {
	provider, model, stage, category, code string
	status                                 int
}

func (b *spendLimit) failed(f dispatchFailure) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = append(b.failures, f)
}

func modelFailure(provider, model string, err error) dispatchFailure {
	f := dispatchFailure{provider: "unlisted", model: "unlisted", stage: "send", category: "unclassified"}
	if provider == "openai" || provider == "anthropic" {
		f.provider = provider
	}
	if _, found := catalog.Find(provider, model); found {
		f.model = model
	}
	if failure, ok := errors.AsType[*llm.Error](err); ok {
		switch failure.Category {
		case llm.ErrorInvalidRequest, llm.ErrorUnsupported, llm.ErrorAuthentication,
			llm.ErrorRateLimited, llm.ErrorOverloaded:
			f.stage, f.category = "provider_response", string(failure.Category)
		case llm.ErrorTransport:
			f.stage, f.category = "transport", "transport"
		case llm.ErrorProtocol:
			f.stage, f.category = "response_decoding", "protocol"
		}
		if failure.Status >= 100 && failure.Status <= 599 {
			f.status = failure.Status
		}
		// Provider codes are untrusted strings. Publish known machine codes only.
		switch failure.Code {
		case "invalid_api_key", "model_not_found", "permission_denied", "invalid_request_error",
			"unsupported_parameter", "unsupported_value", "context_length_exceeded",
			"rate_limit_exceeded", "insufficient_quota", "server_error", "overloaded_error":
			f.code = failure.Code
		}
	}
	if errors.Is(err, llm.ErrNoAPIKey) {
		f.stage, f.category = "configuration", "authentication"
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		f.stage, f.category = "transport", "context_ended"
	}
	return f
}

// limitedClient composes the framework's request cost bound with one
// run-wide ledger. Prepare still performs no network access.
type limitedClient struct {
	llm.Client
	limit     *spendLimit
	preflight bool
	reply     string
}

func (c *limitedClient) Prepare(ctx context.Context, input llm.Input) (llm.Request, error) {
	request, err := c.Client.Prepare(ctx, input)
	if err != nil {
		failure := modelFailure(c.Client.Provider(), input.Model, err)
		failure.stage = "preparation"
		c.limit.failed(failure)
		return request, err
	}
	snapshot := request.Snapshot()
	if snapshot.Origin.Provider == "openai" && snapshot.Origin.API == "openai.responses" {
		// An omitted tier follows project settings, which may select premium
		// pricing. Pin the standard tier before computing the exact body bound.
		var body map[string]json.RawMessage
		if err := json.Unmarshal(snapshot.Body, &body); err != nil {
			return llm.Request{}, errors.New("cannot encode evaluation serving tier")
		}
		body["service_tier"] = json.RawMessage(`"default"`)
		snapshot.Body, err = json.Marshal(body)
		if err != nil {
			return llm.Request{}, errors.New("cannot encode evaluation serving tier")
		}
		return llm.NewRequest(snapshot)
	}
	return request, nil
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
	// The Responses adapter does not yet lift this scalar into Result.
	// Parse only tier metadata, never provider error bodies or text.
	if snapshot.Origin.Provider == "openai" && result.ServiceTier == "" {
		var fields struct {
			ServiceTier string `json:"service_tier"`
		}
		if json.Unmarshal(result.Raw, &fields) == nil {
			result.ServiceTier = fields.ServiceTier
		}
	}
	actual, priced := price.Estimate(result.Usage)
	served := result.Served.Model == snapshot.Origin.Model
	if entry, found := catalog.Find(snapshot.Origin.Provider, snapshot.Origin.Model); found {
		served = entry.Serves(result.Served.Model)
	}
	tier := (snapshot.Origin.Provider == "openai" && result.ServiceTier == "default") ||
		(snapshot.Origin.Provider == "anthropic" && result.ServiceTier == "standard")
	known := priced && !actual.Partial() && result.Served.Provider == snapshot.Origin.Provider && served && tier && actual.Total <= cost.Total
	if err != nil {
		c.limit.failed(modelFailure(snapshot.Origin.Provider, snapshot.Origin.Model, err))
	} else if !known {
		failure := modelFailure(snapshot.Origin.Provider, snapshot.Origin.Model, nil)
		failure.stage = "charge_verification"
		switch {
		case !priced:
			failure.category = "usage_unpriced"
		case actual.Partial():
			failure.category = "unpriced_tool_charge"
		case result.Served.Provider != snapshot.Origin.Provider || !served:
			failure.category = "served_identity"
		case !tier:
			failure.category = "service_tier"
		default:
			failure.category = "cost_above_reserve"
		}
		c.limit.failed(failure)
	}
	c.limit.finish(cost.Total, actual.Total, known && err == nil, known && err == nil && completedText(result, snapshot.Origin.Provider, snapshot.Origin.Model))
	if err == nil && !known {
		return result, errUnknownCharge
	}
	return result, err
}

// Settlement is stricter than measurement: incomplete results and malformed
// content retain their entire risk even when their token usage can be priced.
func completedText(result llm.Result, provider, model string) bool {
	if result.Stop.Kind != llm.StopFinished || result.Message.Role != llm.RoleAssistant ||
		result.Requested.Provider != provider || result.Requested.Model != model ||
		result.Usage == nil || !result.Usage.RequestsComplete || len(result.Message.Content) == 0 {
		return false
	}
	for _, part := range result.Message.Content {
		if part.Kind != llm.ContentBlock || part.Block == nil || part.Block.Text == nil || part.Validate() != nil {
			return false
		}
	}
	return true
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
	limit     *spendLimit
	preflight bool
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
	if !t.preflight {
		if err != nil {
			failure := dispatchFailure{provider: "typesafe", model: decisionModel, stage: "send", category: "unclassified"}
			if api, ok := errors.AsType[*decide.APIError](err); ok {
				failure.stage, failure.category = "provider_response", "http_error"
				if api.StatusCode >= 100 && api.StatusCode <= 599 {
					failure.status = api.StatusCode
				}
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				failure.stage, failure.category = "transport", "context_ended"
			}
			t.limit.failed(failure)
		}
		if resp == nil {
			t.limit.finish(bound, 0, false, false)
		} else {
			known := err == nil && resp.Model == decisionModel && decideUsageComplete(resp) && resp.Usage.InputTokens >= 0 && money.Amount(resp.Usage.InputTokens)*42 <= bound
			// Keep Decide bounds in full; they are small and answer validation
			// happens in the SDK after this transport returns.
			t.limit.finish(bound, money.Amount(resp.Usage.InputTokens)*42, known, false)
			if err == nil && !known {
				t.limit.failed(dispatchFailure{provider: "typesafe", model: decisionModel, stage: "charge_verification", category: "unverified"})
				return resp, errUnknownCharge
			}
		}
	}
	return resp, err
}

func decideUsageComplete(resp *decide.Response) bool {
	var raw struct {
		Usage *struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	return json.Unmarshal(resp.Raw, &raw) == nil && raw.Usage != nil && raw.Usage.InputTokens != nil &&
		raw.Usage.OutputTokens != nil && *raw.Usage.InputTokens == resp.Usage.InputTokens &&
		*raw.Usage.OutputTokens == resp.Usage.OutputTokens && resp.Usage.OutputTokens >= 0
}

// singleDispatchHTTP disables reuse and HTTP/2 connection retries. The SDK
// and model runners add no retries, so every reservation covers one dispatch.
func singleDispatchHTTP() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// Clone initializes the default transport's HTTP/2 configuration. Remove
	// its copied ALPN advertisement as well as the HTTP/2 handler.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = new(tls.Config)
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
