package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

type clientConfig struct {
	id, endpoint, key string
	models            map[string]llm.ModelInfo
	unknown           llm.UnknownPolicy
}

func TestCustomEndpointRequiresIdentity(t *testing.T) {
	if _, err := responses.New(responses.Config{Endpoint: "https://example.com/responses"}); err == nil {
		t.Fatal("Responses accepted custom endpoint without identity")
	}
	if _, err := messages.New(messages.Config{Endpoint: "https://example.com/messages"}); err == nil {
		t.Fatal("Messages accepted custom endpoint without identity")
	}
	openai := newResponses(t, responses.Config{APIKey: "test"})
	anthropic := newMessages(t, messages.Config{APIKey: "test"})
	if openai.Provider() != "openai" || openai.API() != responses.API || anthropic.Provider() != "anthropic" || anthropic.API() != messages.API {
		t.Fatal("default service identity was conflated with API format")
	}
}

func TestRequestPinsTransportIndependentlyOfSink(t *testing.T) {
	for _, tc := range cases(t) {
		for _, mode := range []struct {
			name    string
			stream  bool
			observe bool
		}{
			{name: "complete without sink"},
			{name: "complete with sink", observe: true},
			{name: "stream without sink", stream: true},
			{name: "stream with sink", stream: true, observe: true},
		} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				stream, observe := mode.stream, mode.observe
				var sent []byte
				var accept string
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					sent, _ = io.ReadAll(r.Body)
					accept = r.Header.Get("Accept")
					if stream {
						io.WriteString(w, tc.stream)
					} else {
						io.WriteString(w, tc.complete)
					}
				})
				p := tc.new(server.URL)
				prep := preparedRequest(t, p, stream)
				var wire struct{ Stream bool }
				if err := json.Unmarshal(prep.Body(), &wire); err != nil || wire.Stream != stream {
					t.Fatal("preparation did not pin stream mode", err)
				}
				body := prep.Body()
				prep.Body()[0] = '['
				copy := prep.Snapshot()
				copy.Input.Stream = !stream
				copy.Body[0] = '['
				var seen int
				var sink llm.Sink
				if observe {
					sink = func(llm.Delta) {
						seen++
					}
				}
				result, err := p.Send(context.Background(), prep, sink)
				if err != nil || result.Message.Text() == "" {
					t.Fatal("collection depended on observation", err)
				}
				if !bytes.Equal(sent, body) || (accept == "text/event-stream") != stream {
					t.Fatal("Send changed prepared transport or payload")
				}
				if (seen > 0) != (stream && observe) {
					t.Fatal("sink selected transport mode")
				}
			})
		}
	}
}

func TestRequestPreservesRequestedAndEffectiveSettings(t *testing.T) {
	p := newMessages(t, messages.Config{APIKey: "test", UnknownCapabilities: llm.UnknownAllow})
	r := modelInput()
	prep, err := p.Prepare(context.Background(), r)
	if err != nil {
		t.Fatalf("prepare request with omitted settings: %v", err)
	}
	if r.Settings.MaxTokens != 0 || prep.Input().Settings.MaxTokens != 0 || prep.Effective().MaxTokens != 1024 || len(prep.Adjustments()) != 1 {
		t.Fatal("preparation lost original omission or effective default")
	}
	if prep.Snapshot().ModelInfo != nil || !slices.Contains(prep.Snapshot().Unverified, llm.FeatureText) {
		t.Fatal("unknown model knowledge was presented as verified")
	}
	encoded, err := json.Marshal(prep.Snapshot())
	if err != nil {
		t.Fatalf("encode preparation evidence: %v", err)
	}
	var record llm.RequestSnapshot
	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatalf("decode preparation evidence: %v", err)
	}
	restored, err := llm.NewRequest(record)
	if err != nil || !bytes.Equal(restored.Body(), prep.Body()) || restored.Origin() != prep.Origin() {
		t.Fatal("preparation evidence could not round trip", err)
	}
}

func TestPrepareChecksModelKnowledgeWithoutNetwork(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
			})
			p := configured(t, tc, clientConfig{endpoint: server.URL})
			if _, known := p.ModelInfo("requested"); known {
				t.Fatal("unknown model reported as known")
			}
			if _, err := p.Prepare(context.Background(), modelInput()); err == nil {
				t.Fatal("strict default accepted unknown model")
			}
			info := textModel(tc, server.URL)
			catalog := map[string]llm.ModelInfo{"requested": info}
			strict := configured(t, tc, clientConfig{endpoint: server.URL, models: catalog})
			if _, err := strict.Prepare(context.Background(), modelInput()); err != nil {
				t.Fatal("known supported model rejected", err)
			}
			r := modelInput()
			temperature := 0.5
			r.Settings.Temperature = &temperature
			if _, err := strict.Prepare(context.Background(), r); err == nil || !strings.Contains(err.Error(), "temperature") {
				t.Fatal("unknown setting support accepted", err)
			}
			allow := configured(t, tc, clientConfig{endpoint: server.URL, models: catalog, unknown: llm.UnknownAllow})
			prep, err := allow.Prepare(context.Background(), r)
			if err != nil || !slices.Equal(prep.Snapshot().Unverified, []llm.Feature{llm.FeatureTemperature}) {
				t.Fatal("passthrough did not record unverified setting", err)
			}
			info.Capabilities.Features[llm.FeatureTemperature] = llm.SupportUnsupported
			catalog["requested"] = info
			deny := configured(t, tc, clientConfig{endpoint: server.URL, models: catalog, unknown: llm.UnknownAllow})
			if _, err := deny.Prepare(context.Background(), r); err == nil {
				t.Fatal("passthrough overrode known unsupported setting")
			}
			info.Capabilities.Features[llm.FeatureTemperature] = llm.SupportSupported
			info.Capabilities.MaxOutputTokens = 100
			catalog["requested"] = info
			limited := configured(t, tc, clientConfig{endpoint: server.URL, models: catalog})
			r.Settings.MaxTokens = 101
			if _, err := limited.Prepare(context.Background(), r); err == nil || !strings.Contains(err.Error(), "max_tokens") {
				t.Fatal("known output limit ignored", err)
			}
			if calls.Load() != 0 {
				t.Fatal("preparation accessed network")
			}
		})
	}
}

func TestModelKnowledgeIsOwnedAndRecorded(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			info := textModel(tc, "https://example.com/api")
			info.Pricing = &llm.Pricing{
				Currency:  "USD",
				UpdatedAt: "2026-09-27",
				Base:      llm.Rates{Input: 2},
				Tiers: []llm.PriceTier{{
					MinInputTokens: 100,
					Rates:          llm.Rates{Input: 4},
				}},
				Modality: map[string]llm.Rates{
					"audio": {Input: 3},
				},
			}
			catalog := map[string]llm.ModelInfo{"requested": info}
			p := configured(t, tc, clientConfig{endpoint: "https://example.com/api", models: catalog})
			info.Capabilities.Features[llm.FeatureText] = llm.SupportUnsupported
			info.Pricing.Modality["audio"] = llm.Rates{Input: 100}
			info.Pricing.Tiers[0].Rates.Input = 100
			delete(catalog, "requested")
			copy, _ := p.ModelInfo("requested")
			copy.Capabilities.Features[llm.FeatureText] = llm.SupportUnsupported
			copy.Pricing.Base.Input = 200
			copy.Pricing.Tiers[0].Rates.Input = 200
			prep, err := p.Prepare(context.Background(), modelInput())
			if err != nil {
				t.Fatal("catalog snapshots were shared", err)
			}
			record := prep.Snapshot()
			if record.ModelInfo.Evidence.Source != "test evidence" ||
				record.ModelInfo.Pricing.Base.Input != 2 ||
				record.ModelInfo.Pricing.Tiers[0].Rates.Input != 4 ||
				record.ModelInfo.Pricing.Modality["audio"].Input != 3 ||
				len(record.Unverified) != 0 {
				t.Fatal("prepared evidence lost model knowledge or pricing")
			}
			record.ModelInfo.Pricing.Base.Input = 400
			if prep.Snapshot().ModelInfo.Pricing.Base.Input != 2 {
				t.Fatal("record mutation changed prepared evidence")
			}
		})
	}
}

func TestPrivateReplayRequiresMatchingOriginUnlessCanonicalConversionIsSupported(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			response := tc.complete
			if tc.name == "openai" {
				response = `{"object":"response","status":"completed","model":"served","output":[` +
					`{"type":"reasoning","encrypted_content":"private","summary":[]},` +
					`{"type":"message","id":"m","content":[{"type":"output_text","text":"hello","annotations":[{"type":"url_citation","url":"https://example.com"}]}]},` +
					`{"type":"function_call","call_id":"call","name":"search","arguments":"{}"},` +
					`{"type":"future_item","private":"retained"}]}`
			}
			var calls atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, response)
			})
			base := clientConfig{id: "shared-name", endpoint: server.URL, key: "credential-one", unknown: llm.UnknownAllow}
			p := configured(t, tc, base)
			prep := preparedRequest(t, p)
			result, err := p.Send(context.Background(), prep, nil)
			if err != nil {
				t.Fatalf("collect response for replay checks: %v", err)
			}
			if result.Requested != (llm.ModelRef{Provider: base.id, Model: "requested"}) || result.Served.Provider != base.id {
				t.Fatal("result lost configured provider identity")
			}
			for _, variant := range []string{"same", "identity", "endpoint", "credential", "model"} {
				t.Run(variant, func(t *testing.T) {
					config := base
					model := "requested"
					switch variant {
					case "identity":
						config.id = "other"
					case "endpoint":
						config.endpoint += "/other"
					case "credential":
						config.key = "credential-two"
					case "model":
						model = "other"
					}
					other := configured(t, tc, config)
					for _, part := range result.Message.Content {
						r := modelInput()
						r.Model = model
						r.Messages = append(r.Messages, llm.AssistantMessage(part))
						r.Messages = append(r.Messages, toolReplies(llm.AssistantMessage(part))...)
						_, err := other.Prepare(context.Background(), r)
						if (err == nil) != (variant == "same" || part.ToolCall != nil) {
							t.Fatalf("%s replay: %v", part.Kind, err)
						}
						// Citations must also carry origin when separated from their
						// enclosing provider data by an application projection.
						if part.Block != nil && part.Block.Text != nil && len(part.Block.Text.Citations) > 0 {
							part.Replay = nil
							r.Messages[len(r.Messages)-1] = llm.AssistantMessage(part)
							_, err := other.Prepare(context.Background(), r)
							if (err == nil) != (variant == "same") {
								t.Fatal("citation replay bypassed origin", err)
							}
						}
					}
					if variant != "same" && variant != "model" {
						if _, err := other.Send(context.Background(), prep, nil); err == nil {
							t.Fatal("prepared request crossed destination boundary")
						}
					}
				})
			}
			if calls.Load() != 1 {
				t.Fatal("mismatched destination sent a request")
			}
			if strings.Contains(mustJSON(prep.Snapshot()), base.key) {
				t.Fatal("preparation evidence exposed credentials")
			}
		})
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/second" {
					io.WriteString(w, tc.complete)
					return
				}
				http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
			})
			p := tc.new(server.URL)
			_, err := p.Send(context.Background(), preparedRequest(t, p), nil)
			var failure *llm.Error
			if !errors.As(err, &failure) || failure.Status != 307 || failure.Retryable || calls.Load() != 1 {
				t.Fatal("redirect hid a second dispatch", err, calls.Load())
			}
		})
	}
}

func TestRequestAndReplayRejectOtherAPI(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			p, result := completeCall(t, tc)
			prep := preparedRequest(t, p)
			record := prep.Snapshot()
			record.Origin.API = "other.api"
			foreign, err := llm.NewRequest(record)
			if err != nil {
				t.Fatalf("construct prepared request for API mismatch check: %v", err)
			}
			if _, err := p.Send(context.Background(), foreign, nil); err == nil {
				t.Fatal("prepared request crossed API boundary")
			}
			for _, part := range result.Message.Content {
				if part.Replay != nil {
					part.Replay.Origin.API = "other.api"
				}
				if part.Opaque != nil {
					part.Opaque.Origin.API = "other.api"
				}
				r := modelInput()
				r.Messages = append(r.Messages, llm.AssistantMessage(part))
				r.Messages = append(r.Messages, toolReplies(llm.AssistantMessage(part))...)
				if _, err := p.Prepare(context.Background(), r); err == nil {
					t.Fatal("private replay crossed API boundary")
				}
			}
		})
	}
}

func TestRetryHintsPreserveRateLimitAndQuotaMeaning(t *testing.T) {
	for _, tc := range cases(t) {
		for _, code := range []string{"slow_down", "insufficient_quota"} {
			t.Run(tc.name+"/"+code, func(t *testing.T) {
				var calls atomic.Int32
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(429)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
				})
				p := tc.new(server.URL)
				_, err := p.Send(context.Background(), preparedRequest(t, p), nil)
				var failure *llm.Error
				if !errors.As(err, &failure) || failure.Code != code || failure.RetryAfter != 7*time.Second || failure.Retryable != (code == "slow_down") || calls.Load() != 1 {
					t.Fatal("retry hint lost provider meaning or triggered retry", err)
				}
			})
		}
	}
}

func configured(t *testing.T, tc providerCase, c clientConfig) llm.Client {
	t.Helper()
	if c.id == "" {
		c.id = tc.name
	}
	if c.key == "" {
		c.key = "test"
	}
	if tc.name == "anthropic" {
		return newMessages(t, messages.Config{Provider: c.id, Endpoint: c.endpoint, APIKey: c.key, Models: c.models, UnknownCapabilities: c.unknown})
	}
	return newResponses(t, responses.Config{Provider: c.id, Endpoint: c.endpoint, APIKey: c.key, Models: c.models, UnknownCapabilities: c.unknown})
}

func textModel(tc providerCase, endpoint string) llm.ModelInfo {
	api := responses.API
	if tc.name == "anthropic" {
		api = messages.API
	}
	return llm.ModelInfo{
		Evidence: llm.ModelEvidence{Model: "requested", API: api, Endpoint: endpoint,
			Owner: "Kernel tests", Source: "test evidence", Method: llm.EvidenceFixture,
			QualifiedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)},
		Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{
			llm.FeatureText: llm.SupportSupported, llm.FeatureMaxTokens: llm.SupportSupported,
		}},
	}
}
