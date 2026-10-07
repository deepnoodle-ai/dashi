package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestReservationsAreAtomicAndFailuresNeverRelease(t *testing.T) {
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
	b.finish(0, 0, false, false)
	if admitted.Load() != 90 || b.snapshot().reserved != b.cap || b.reserve(1) == nil {
		t.Fatalf("admitted %d, spending %+v", admitted.Load(), b.snapshot())
	}
}

func TestSuccessfulSettlementIsAtomicAndKeepsFailedRisk(t *testing.T) {
	b, _ := newSpendLimit("0.09")
	bound, actual := money.MustParseAmount("0.01"), money.MustParseAmount("0.001")
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if b.reserve(bound) != nil {
				return
			}
			admitted.Add(1)
			b.finish(bound, actual, true, true)
			if got := b.snapshot(); got.reserved > got.cap {
				t.Error("settlement allowed an overspend")
			}
		})
	}
	wg.Wait()
	n := money.Amount(admitted.Load())
	got := b.snapshot()
	if n > 90 || got.reserved != n*actual || got.observed != n*actual || got.fullReserved != n*bound || got.settled != int(n) {
		t.Fatalf("settlement %+v, admitted %d", got, n)
	}
	// A known incomplete call and an ambiguous failure each keep their bound.
	b, _ = newSpendLimit("0.09")
	if b.reserve(bound) != nil || b.reserve(bound) != nil {
		t.Fatal("initial reservations rejected")
	}
	b.finish(bound, actual, true, false)
	b.finish(bound, 0, false, false)
	got = b.snapshot()
	if got.reserved != 2*bound || got.observed != actual || got.unknown != 1 || b.reserve(1) == nil {
		t.Fatal("incomplete or unknown risk was released")
	}
}

type resultModel struct {
	llm.Client
	result llm.Result
	err    error
}

func (c resultModel) Send(context.Context, llm.Call) (llm.Result, error) { return c.result, c.err }

func TestModelSettlementRequiresCompleteVerifiedSuccessfulResult(t *testing.T) {
	provider, _ := responses.New(responses.Config{})
	request, err := provider.Prepare(context.Background(), llm.Input{Model: "gpt-6-luna", Settings: llm.Settings{MaxTokens: 512}, Messages: []llm.Message{llm.UserMessage(llm.Text("test"))}})
	if err != nil {
		t.Fatal(err)
	}
	bound, _ := llm.WorstCaseCost(request, *request.Snapshot().ModelInfo.Pricing)
	for _, tc := range []struct {
		name    string
		settles bool
		change  func(*llm.Result) error
	}{
		{"complete", true, func(*llm.Result) error { return nil }},
		{"incomplete usage", false, func(r *llm.Result) error { r.Raw = []byte(`{"usage":{}}`); return nil }},
		{"tool counters incomplete", true, func(r *llm.Result) error { r.Usage.RequestsComplete = false; return nil }},
		{"output limit", false, func(r *llm.Result) error { r.Stop.Kind = llm.StopOutputLimit; return nil }},
		{"failed", false, func(*llm.Result) error { return errors.New("failed") }},
		{"unknown usage", false, func(r *llm.Result) error { r.Usage = nil; return nil }},
		{"served model", false, func(r *llm.Result) error { r.Served.Model = "other"; return nil }},
		{"tier", false, func(r *llm.Result) error { r.ServiceTier = "priority"; return nil }},
		{"missing tier", false, func(r *llm.Result) error { r.ServiceTier = ""; return nil }},
		{"invalid usage", false, func(r *llm.Result) error { r.Usage.InputTokens = -1; return nil }},
		{"wrong requested", false, func(r *llm.Result) error { r.Requested.Model = "other"; return nil }},
		{"invalid content", false, func(r *llm.Result) error { r.Message.Role = llm.RoleUser; return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := llm.ModelRef{Provider: "openai", Model: "gpt-6-luna"}
			result := llm.Result{Requested: ref, Served: ref, ServiceTier: "default", Raw: []byte(`{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}}`), Stop: llm.Stop{Kind: llm.StopFinished}, Message: llm.AssistantMessage(llm.Text("answer")), Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, RequestsComplete: true}}
			base := resultModel{Client: provider, err: tc.change(&result)}
			base.result = result
			limit := &spendLimit{cap: money.MustParseAmount("0.1")}
			guard := &limitedClient{Client: base, limit: limit}
			guard.Send(context.Background(), llm.Call{Request: request})
			got := limit.snapshot()
			if tc.settles {
				if got.settled != 1 || got.reserved != got.observed || got.reserved >= bound.Total || got.fullReserved != bound.Total {
					t.Fatalf("successful settlement %+v", got)
				}
			} else if got.settled != 0 || got.reserved != bound.Total {
				t.Fatalf("unverified result released risk %+v", got)
			}
		})
	}
}

func TestOpenAIUsesAndVerifiesDefaultServingTier(t *testing.T) {
	provider, _ := responses.New(responses.Config{})
	limit := &spendLimit{cap: money.MustParseAmount("0.1")}
	ref := llm.ModelRef{Provider: "openai", Model: "gpt-6-luna"}
	result := llm.Result{Requested: ref, Served: ref, ServiceTier: "default", Raw: []byte(`{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}},"output":"PRIVATE"}`), Stop: llm.Stop{Kind: llm.StopFinished}, Message: llm.AssistantMessage(llm.Text("answer")), Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, RequestsComplete: true}}
	guard := &limitedClient{Client: resultModel{Client: provider, result: result}, limit: limit}
	request, err := guard.Prepare(context.Background(), llm.Input{Model: "gpt-6-luna", Settings: llm.Settings{MaxTokens: 512}, Messages: []llm.Message{llm.UserMessage(llm.Text("test"))}})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ServiceTier string `json:"service_tier"`
	}
	if json.Unmarshal(request.Body(), &body) != nil || body.ServiceTier != "default" {
		t.Fatal("request could inherit project premium pricing")
	}
	got, err := guard.Send(context.Background(), llm.Call{Request: request})
	if err != nil || got.ServiceTier != "default" || limit.snapshot().settled != 1 {
		t.Fatal("reported default tier was not verified")
	}
}

func TestRealResponsesSettlementRequiresCompleteTokenEvidence(t *testing.T) {
	complete := `{"input_tokens":20,"output_tokens":2,"total_tokens":22,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}`
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, usage, tier, model string
			success                  bool
		}{
			{"complete", complete, "default", "gpt-6-luna", true},
			{"missing usage", `null`, "default", "gpt-6-luna", false},
			{"empty usage", `{}`, "default", "gpt-6-luna", false},
			{"missing input", `{"output_tokens":2,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, "default", "gpt-6-luna", false},
			{"missing output", `{"input_tokens":20,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, "default", "gpt-6-luna", false},
			{"missing cache", `{"input_tokens":20,"output_tokens":2}`, "default", "gpt-6-luna", false},
			{"missing cache write", `{"input_tokens":20,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}`, "default", "gpt-6-luna", false},
			{"missing cache read", `{"input_tokens":20,"output_tokens":2,"input_tokens_details":{"cache_write_tokens":0}}`, "default", "gpt-6-luna", false},
			{"negative cache", `{"input_tokens":20,"output_tokens":2,"input_tokens_details":{"cached_tokens":-1,"cache_write_tokens":0}}`, "default", "gpt-6-luna", false},
			{"total mismatch", `{"input_tokens":20,"output_tokens":2,"total_tokens":23,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, "default", "gpt-6-luna", false},
			{"priority", complete, "priority", "gpt-6-luna", false},
			{"missing tier", complete, "", "gpt-6-luna", false},
			{"wrong served model", complete, "default", "other-model", false},
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, tc.name), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request struct {
						ServiceTier string `json:"service_tier"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil || request.ServiceTier != "default" {
						t.Error("wire request did not pin standard pricing")
					}
					response := fmt.Sprintf(`{"object":"response","status":"completed","model":%q,"service_tier":%q,"usage":%s,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, tc.model, tc.tier, tc.usage)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, response)
					}
				}))
				defer server.Close()
				public, _ := responses.New(responses.Config{})
				info, _ := public.ModelInfo("gpt-6-luna")
				info.Evidence.Endpoint = server.URL
				provider, err := responses.New(responses.Config{Provider: "openai", APIKey: "local-key", Endpoint: server.URL, Client: singleDispatchHTTP(), Models: map[string]llm.ModelInfo{"gpt-6-luna": info}})
				if err != nil {
					t.Fatal(err)
				}
				limit := &spendLimit{cap: money.MustParseAmount("0.1")}
				guard := &limitedClient{Client: provider, limit: limit}
				request, err := guard.Prepare(context.Background(), llm.Input{Model: "gpt-6-luna", Stream: stream, Settings: llm.Settings{MaxTokens: 512}, Messages: []llm.Message{llm.UserMessage(llm.Text("test"))}})
				if err != nil {
					t.Fatal(err)
				}
				bound, _ := llm.WorstCaseCost(request, *request.Snapshot().ModelInfo.Pricing)
				_, err = guard.Send(context.Background(), llm.Call{Request: request})
				got := limit.snapshot()
				if tc.success {
					if err != nil || got.settled != 1 || got.observed <= 0 || got.reserved != got.observed || got.fullReserved != bound.Total {
						t.Fatalf("complete real result did not settle: %v, %+v", err, got)
					}
				} else {
					if !errors.Is(err, errUnknownCharge) || got.settled != 0 || got.reserved != bound.Total || got.unknown != 1 {
						t.Fatalf("incomplete real result released risk: %v, %+v", err, got)
					}
					guard.Send(context.Background(), llm.Call{Request: request})
				}
				if calls.Load() != 1 {
					t.Fatal("failed call was dispatched again")
				}
			})
		}
	}
}

type countedModel struct {
	llm.Client
	calls int
}

type unknownUsageModel struct {
	llm.Client
	calls int
}

func (c *unknownUsageModel) Send(context.Context, llm.Call) (llm.Result, error) {
	c.calls++
	return llm.Result{}, nil
}

func TestUnknownChargeCancelsRunAndPreventsMoreDispatch(t *testing.T) {
	provider, _ := responses.New(responses.Config{})
	request, err := provider.Prepare(context.Background(), llm.Input{Model: "gpt-6-luna", Settings: llm.Settings{MaxTokens: 64}, Messages: []llm.Message{llm.UserMessage(llm.Text("test"))}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := &unknownUsageModel{Client: provider}
	limit := &spendLimit{cap: money.MustParseAmount("0.99"), cancel: cancel}
	guard := &limitedClient{Client: base, limit: limit}
	if _, err := guard.Send(ctx, llm.Call{Request: request}); !errors.Is(err, errUnknownCharge) {
		t.Fatalf("unknown charge error %v", err)
	}
	if ctx.Err() == nil || limit.snapshot().reserved <= 0 {
		t.Fatal("unknown charge did not cancel or retain its reserve")
	}
	if _, err := guard.Send(context.Background(), llm.Call{Request: request}); !errors.Is(err, errUnknownCharge) || base.calls != 1 {
		t.Fatal("another dispatch followed an unknown charge")
	}
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
	if _, err := guard.Send(context.Background(), llm.Call{Request: request}); err == nil || base.calls != 1 {
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

func TestSingleDispatchNegotiatesHTTP1AndDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.ProtoMajor != 1 || r.TLS.NegotiatedProtocol != "http/1.1" {
			t.Errorf("unexpected negotiated protocol %s / %s", r.Proto, r.TLS.NegotiatedProtocol)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	client := singleDispatchHTTP()
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	for range 2 {
		response, err := client.Post(server.URL, "application/json", strings.NewReader(`{"test":true}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("unexpected status %d", response.StatusCode)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("two invocations made %d HTTP requests", calls.Load())
	}
}

func TestModelFailureMetadataDistinguishesStagesAndOmitsPrivateText(t *testing.T) {
	secret := "PRIVATE-session-id-or-body"
	for _, tc := range []struct {
		name, stage, category, code string
		status                      int
		err                         error
	}{
		{"request rejection", "provider_response", "invalid_request", "unsupported_parameter", 400, &llm.Error{Category: llm.ErrorInvalidRequest, Status: 400, Code: "unsupported_parameter", Body: []byte(secret), Cause: errors.New(secret)}},
		{"authentication", "provider_response", "authentication", "invalid_api_key", 401, &llm.Error{Category: llm.ErrorAuthentication, Status: 401, Code: "invalid_api_key", Body: []byte(secret)}},
		{"overloaded", "provider_response", "overloaded", "server_error", 503, &llm.Error{Category: llm.ErrorOverloaded, Status: 503, Code: "server_error"}},
		{"transport", "transport", "transport", "", 0, &llm.Error{Category: llm.ErrorTransport, Cause: errors.New(secret)}},
		{"decode", "response_decoding", "protocol", "", 0, &llm.Error{Category: llm.ErrorProtocol, Cause: errors.New(secret)}},
		{"missing key", "configuration", "authentication", "", 0, &llm.Error{Category: llm.ErrorAuthentication, Cause: llm.ErrNoAPIKey}},
		{"untrusted code", "provider_response", "invalid_request", "", 400, &llm.Error{Category: llm.ErrorInvalidRequest, Status: 400, Code: secret}},
		{"unclassified", "send", "unclassified", "", 0, errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := modelFailure("openai", "gpt-6-luna", tc.err)
			if f.stage != tc.stage || f.category != tc.category || f.status != tc.status || f.code != tc.code {
				t.Fatalf("metadata %+v", f)
			}
			if f.provider != "openai" || f.model != "gpt-6-luna" {
				t.Fatal("lost public requested model")
			}
			var out strings.Builder
			(&report{spending: &spending{failures: []dispatchFailure{f}}}).print(&out, config{})
			if strings.Contains(out.String(), secret) {
				t.Fatal("report exposed private error text")
			}
		})
	}
	f := modelFailure(secret, secret, errors.New(secret))
	if f.provider != "unlisted" || f.model != "unlisted" {
		t.Fatal("unlisted identity was published")
	}
}
