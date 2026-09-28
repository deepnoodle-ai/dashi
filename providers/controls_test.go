package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestPrepareControlsMatchDispatchedBytes(t *testing.T) {
	zero, parallel := 0.0, false
	for _, tc := range cases(t) {
		for _, mode := range []struct {
			name   string
			stream bool
		}{{name: "complete"}, {name: "stream", stream: true}} {
			for _, setting := range []struct {
				name     string
				settings llm.Settings
			}{
				{name: "defaults"},
				{
					name: "explicit zero and false",
					settings: llm.Settings{
						MaxTokens:     2048,
						Temperature:   &zero,
						ParallelTools: &parallel,
					},
				},
				{
					name: "native budget",
					settings: llm.Settings{
						Reasoning: &llm.ReasoningSettings{BudgetTokens: 2048},
					},
				},
				{
					name:     "native effort",
					settings: llm.Settings{Reasoning: &llm.ReasoningSettings{Effort: "low"}},
				},
				{
					name: "forced tool",
					settings: llm.Settings{
						ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceTool, Name: "act"},
						ParallelTools: &parallel,
					},
				},
			} {
				if tc.name == "openai" && setting.name == "native budget" {
					continue
				}
				t.Run(tc.name+"/"+mode.name+"/"+setting.name, func(t *testing.T) {
					var sent []byte
					server := serve(
						t,
						func(w http.ResponseWriter, r *http.Request) {
							sent, _ = io.ReadAll(r.Body)
							if mode.stream {
								io.WriteString(w, tc.stream)
							} else {
								io.WriteString(w, tc.complete)
							}
						},
					)
					info := controlsModel(tc, server.URL)
					catalog := map[string]llm.ModelInfo{"requested": info}
					var p llm.Client
					if tc.name == "anthropic" {
						p = newMessages(
							t,
							messages.Config{
								Provider:         tc.name,
								APIKey:           "test",
								Endpoint:         server.URL,
								DefaultMaxTokens: 8192,
								Models:           catalog,
							},
						)
					} else {
						p = configured(
							t,
							tc,
							clientConfig{
								endpoint: server.URL,
								models:   catalog,
							},
						)
					}
					r := modelInput()
					r.Stream, r.Settings = mode.stream, setting.settings
					r.Tools = []tool.Definition{
						{Name: "act", Schema: json.RawMessage(`{"type":"object"}`)},
					}
					original := mustJSON(r)
					prep, err := p.Prepare(context.Background(), r)
					if err != nil {
						t.Fatalf("prepare supported controls: %v", err)
					}
					if sent != nil ||
						mustJSON(prep.Input()) != original ||
						mustJSON(r) != original {
						t.Fatal("preparation dispatched or changed requested controls")
					}
					want := map[string]any{"stream": mode.stream}
					if tc.name == "anthropic" {
						limit := r.Settings.MaxTokens
						if limit == 0 {
							limit = 8192
						}
						want["max_tokens"] = float64(limit)
						if prep.Effective().MaxTokens != limit {
							t.Fatal("effective limit differs from configured default or explicit setting")
						}
						if r.Settings.Reasoning != nil {
							if r.Settings.Reasoning.BudgetTokens > 0 {
								want["thinking"] = map[string]any{
									"type":          "enabled",
									"budget_tokens": float64(2048),
								}
							} else {
								want["thinking"] = map[string]any{"type": "adaptive"}
								want["output_config"] = map[string]any{"effort": "low"}
							}
						}
						if r.Settings.ParallelTools != nil {
							choice := map[string]any{
								"type":                      "auto",
								"disable_parallel_tool_use": true,
							}
							if r.Settings.ToolChoice != nil {
								choice["type"], choice["name"] = "tool", "act"
							} else if prep.Effective().ToolChoice == nil ||
								prep.Effective().ToolChoice.Mode != llm.ToolChoiceAuto {
								t.Fatal("implicit tool choice missing from effective settings")
							}
							want["tool_choice"] = choice
						}
					} else {
						want["store"] = false
						want["include"] = []any{"reasoning.encrypted_content"}
						if r.Settings.MaxTokens > 0 {
							want["max_output_tokens"] = float64(r.Settings.MaxTokens)
						}
						if r.Settings.Reasoning != nil {
							want["reasoning"] = map[string]any{"effort": "low"}
						}
						if r.Settings.ParallelTools != nil {
							want["parallel_tool_calls"] = false
						}
						if r.Settings.ToolChoice != nil {
							want["tool_choice"] = map[string]any{"type": "function", "name": "act"}
						}
						if len(prep.Adjustments()) != 0 || !reflect.DeepEqual(
							prep.Effective(),
							r.Settings,
						) {
							t.Fatal("Responses adjusted supported settings")
						}
					}
					if r.Settings.Temperature != nil {
						want["temperature"] = 0.0
					}
					var body map[string]any
					if err := json.Unmarshal(prep.Body(), &body); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"model", "tools", "messages", "input"} {
						delete(body, key)
					}
					if !reflect.DeepEqual(body, want) {
						t.Fatalf(
							"encoded controls = %s, want %s",
							mustJSON(body),
							mustJSON(want),
						)
					}
					if _, err := p.Send(
						context.Background(),
						llm.Call{
							Request: prep,
						},
					); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(sent, prep.Body()) {
						t.Fatal("dispatch changed prepared bytes")
					}
					if prep.Snapshot().ModelInfo.Evidence.Method != llm.EvidenceFixture {
						t.Fatal("local success was promoted to live qualification")
					}
				})
			}
		}
	}
}

func TestPrepareRejectsControlCombinationsOffline(t *testing.T) {
	temperature := 0.5
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := serve(
				t,
				func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
				},
			)
			// adjusts names cases that Messages can fit to a nearby valid
			// request; every other case has no safe substitute.
			for _, test := range []struct {
				name     string
				settings llm.Settings
				prefill  bool
				adjusts  bool
			}{
				{
					name: "effort and budget",
					settings: llm.Settings{
						Reasoning: &llm.ReasoningSettings{Effort: "low", BudgetTokens: 1024},
					},
				},
				{
					name: "unknown effort",
					settings: llm.Settings{
						Reasoning: &llm.ReasoningSettings{Effort: "invented"},
					},
				},
				{
					name:    "budget below minimum",
					adjusts: true,
					settings: llm.Settings{
						MaxTokens: 4096,
						Reasoning: &llm.ReasoningSettings{BudgetTokens: 1023},
					},
				},
				{
					name: "budget equals default limit",
					settings: llm.Settings{
						Reasoning: &llm.ReasoningSettings{BudgetTokens: 1024},
					},
				},
				{
					name:    "budget equals explicit limit",
					adjusts: true,
					settings: llm.Settings{
						MaxTokens: 2048,
						Reasoning: &llm.ReasoningSettings{BudgetTokens: 2048},
					},
				},
				{
					name:    "budget and temperature",
					adjusts: true,
					settings: llm.Settings{
						MaxTokens:   4096,
						Temperature: &temperature,
						Reasoning:   &llm.ReasoningSettings{BudgetTokens: 1024},
					},
				},
				{
					name: "budget and forced tool",
					settings: llm.Settings{
						MaxTokens:  4096,
						ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceAny},
						Reasoning:  &llm.ReasoningSettings{BudgetTokens: 1024},
					},
				},
				{
					name:    "budget and prefill",
					prefill: true,
					settings: llm.Settings{
						MaxTokens: 4096,
						Reasoning: &llm.ReasoningSettings{BudgetTokens: 1024},
					},
				},
			} {
				for _, policy := range []llm.CapabilityPolicy{
					llm.CapabilityAdjust,
					llm.CapabilityStrict,
				} {
					t.Run(
						test.name+"/"+string(policy),
						func(t *testing.T) {
							p := configured(
								t,
								tc,
								clientConfig{
									endpoint: server.URL,
									policy:   policy,
								},
							)
							r := modelInput()
							r.Settings = test.settings
							r.Tools = []tool.Definition{
								{Name: "act", Schema: json.RawMessage(`{}`)},
							}
							if test.prefill {
								r.Messages = append(
									r.Messages,
									llm.AssistantMessage(llm.Text("prefix")),
									llm.Message{
										Role:    llm.RoleSystem,
										Content: []llm.Content{llm.Text("system text")},
									},
								)
							}
							prep, err := p.Prepare(context.Background(), r)
							if test.adjusts &&
								tc.name == "anthropic" &&
								policy == llm.CapabilityAdjust {
								if err != nil {
									t.Fatalf("fittable combination was rejected: %v", err)
								}
								if len(prep.Warnings()) == 0 {
									t.Fatal("fitted combination carried no warning")
								}
								return
							}
							var failure *llm.Error
							if !errors.As(
								err,
								&failure,
							) ||
								(failure.Category != llm.ErrorUnsupported &&
									failure.Category != llm.ErrorInvalidRequest) ||
								len(prep.Body()) != 0 {
								t.Fatalf("invalid combination produced a callable request: %v", err)
							}
						},
					)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("rejected preparation contacted provider")
			}
		})
	}
}

func TestPrepareFitsBudgetBelowConfiguredDefault(t *testing.T) {
	for _, test := range []struct {
		name     string
		limit    int
		budget   int
		accepted bool
	}{
		{name: "package default leaves no room"},
		{name: "configured small", limit: 1500, budget: 1499, accepted: true},
		{name: "configured sufficient", limit: 4096, budget: 2048, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newMessages(
				t,
				messages.Config{
					APIKey:           "test",
					DefaultMaxTokens: test.limit,
				},
			)
			r := modelInput()
			r.Settings.Reasoning = &llm.ReasoningSettings{BudgetTokens: 2048}
			prep, err := p.Prepare(context.Background(), r)
			if (err == nil) != test.accepted {
				t.Fatalf("configured default was not used to fit budget: %v", err)
			}
			if !test.accepted {
				return
			}
			if prep.Effective().Reasoning.BudgetTokens != test.budget {
				t.Fatalf(
					"budget = %d, want %d",
					prep.Effective().Reasoning.BudgetTokens,
					test.budget,
				)
			}
			if prep.Effective().MaxTokens != test.limit {
				t.Fatal("configured default was not applied")
			}
			if clamped := test.budget != 2048; clamped != (len(prep.Warnings()) == 1) {
				t.Fatalf("budget warnings = %+v", prep.Warnings())
			}
		})
	}
}

func TestModelEvidenceMustMatchConfiguredDestination(t *testing.T) {
	for _, tc := range cases(t) {
		for _, test := range []struct {
			name string
			edit func(*llm.ModelEvidence)
		}{
			{
				name: "other API",
				edit: func(e *llm.ModelEvidence) {
					e.API = "other.api"
				},
			},
			{
				name: "other host",
				edit: func(e *llm.ModelEvidence) {
					e.Endpoint = "https://other.example/api"
				},
			},
			{
				name: "other path",
				edit: func(e *llm.ModelEvidence) {
					e.Endpoint += "/other"
				},
			},
			{
				name: "other API version",
				edit: func(e *llm.ModelEvidence) {
					e.Endpoint += "?api-version=other"
				},
			},
			{
				name: "other model",
				edit: func(e *llm.ModelEvidence) {
					e.Model = "other"
				},
			},
		} {
			t.Run(tc.name+"/"+test.name, func(t *testing.T) {
				endpoint := "https://example.com/api"
				info := textModel(tc, endpoint)
				test.edit(&info.Evidence)
				catalog := map[string]llm.ModelInfo{"requested": info}
				var err error
				if tc.name == "anthropic" {
					_, err = messages.New(messages.Config{
						Provider:         "test",
						APIKey:           "test",
						Endpoint:         endpoint,
						Models:           catalog,
						CapabilityPolicy: llm.CapabilityAdjust,
					})
				} else {
					_, err = responses.New(responses.Config{
						Provider:         "test",
						APIKey:           "test",
						Endpoint:         endpoint,
						Models:           catalog,
						CapabilityPolicy: llm.CapabilityAdjust,
					})
				}
				if err == nil {
					t.Fatal("provider silently rebound evidence to a different destination")
				}
			})
		}
	}
}

func TestPrepareRecordsUnknownOutputControl(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "https://example.com/api"
			info := textModel(tc, endpoint)
			delete(info.Capabilities.Features, llm.FeatureMaxTokens)
			catalog := map[string]llm.ModelInfo{"requested": info}
			r := modelInput()
			r.Settings.MaxTokens = 2048
			strict := configured(
				t,
				tc,
				clientConfig{
					endpoint: endpoint,
					models:   catalog,
					policy:   llm.CapabilityStrict,
				},
			)
			if _, err := strict.Prepare(context.Background(), r); err == nil {
				t.Fatal("unknown output control passed strict preparation")
			}
			allow := configured(
				t,
				tc,
				clientConfig{
					endpoint: endpoint,
					models:   catalog,
				},
			)
			prep, err := allow.Prepare(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if prep.Snapshot().CapabilityPolicy != llm.CapabilityAdjust ||
				!slices.Equal(
					prep.Snapshot().Unverified,
					[]llm.Feature{
						llm.FeatureMaxTokens,
					},
				) {
				t.Fatal("output control uncertainty or explicit policy was lost")
			}
		})
	}
}

func TestRequestRemainsPerAttemptAfterDispatchFailure(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [][]byte
			server := serve(
				t,
				func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, body)
					if len(bodies) == 1 {
						http.Error(w, "overloaded", http.StatusServiceUnavailable)
						return
					}
					io.WriteString(w, tc.complete)
				},
			)
			firstInfo := textModel(tc, server.URL)
			secondInfo := firstInfo.Clone()
			secondInfo.Evidence.Model = "fallback"
			p := configured(
				t,
				tc,
				clientConfig{
					endpoint: server.URL,
					models: map[string]llm.ModelInfo{
						"requested": firstInfo,
						"fallback":  secondInfo,
					},
				},
			)
			firstRequest, secondRequest := modelInput(), modelInput()
			firstRequest.Settings.MaxTokens = 2048
			secondRequest.Model, secondRequest.Settings.MaxTokens = "fallback", 4096
			first, err := p.Prepare(context.Background(), firstRequest)
			if err != nil {
				t.Fatal(err)
			}
			second, err := p.Prepare(context.Background(), secondRequest)
			if err != nil {
				t.Fatal(err)
			}
			firstRecord, secondRecord := mustJSON(first.Snapshot()), mustJSON(second.Snapshot())
			if len(bodies) != 0 {
				t.Fatal("preparation dispatched an attempt")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.Send(
				ctx,
				llm.Call{
					Request: first,
				},
			); !errors.Is(err, context.Canceled) || len(bodies) != 0 {
				t.Fatal("preparation or Send invocation was mistaken for dispatch", err)
			}
			if _, err := p.Send(
				context.Background(),
				llm.Call{
					Request: first,
				},
			); err == nil {
				t.Fatal("failed dispatch lost its error")
			}
			result, err := p.Send(context.Background(), llm.Call{Request: second})
			if err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 2 || !bytes.Equal(
				bodies[0],
				first.Body(),
			) || !bytes.Equal(
				bodies[1],
				second.Body(),
			) {
				t.Fatal("attempt bodies were retried or mixed")
			}
			if mustJSON(first.Snapshot()) != firstRecord ||
				mustJSON(second.Snapshot()) != secondRecord ||
				first.Effective().MaxTokens != 2048 ||
				second.Effective().MaxTokens != 4096 ||
				result.Requested.Model != "fallback" ||
				second.Snapshot().ModelInfo.Evidence.Model != "fallback" {
				t.Fatal("dispatch or fallback changed per-attempt facts")
			}
		})
	}
}

func TestPrepareFitsDefaultToModelLimit(t *testing.T) {
	for _, tc := range cases(t) {
		if tc.name != "anthropic" {
			continue
		}
		info := textModel(tc, "https://api.anthropic.com/v1/messages")
		info.Capabilities.MaxOutputTokens = 2048
		models := map[string]llm.ModelInfo{"requested": info}
		strict := newMessages(
			t,
			messages.Config{
				APIKey:           "test",
				DefaultMaxTokens: 4096,
				Models:           models,
				CapabilityPolicy: llm.CapabilityStrict,
			},
		)
		if _, err := strict.Prepare(context.Background(), modelInput()); err == nil {
			t.Fatal("strict policy let a default bypass the model limit")
		}
		p := newMessages(
			t,
			messages.Config{
				APIKey:           "test",
				DefaultMaxTokens: 4096,
				Models:           models,
			},
		)
		prep, err := p.Prepare(context.Background(), modelInput())
		if err != nil {
			t.Fatal(err)
		}
		if prep.Effective().MaxTokens != 2048 {
			t.Fatalf("default max tokens = %d, want 2048", prep.Effective().MaxTokens)
		}
		adjustments := prep.Adjustments()
		if len(adjustments) != 1 || adjustments[0].Action != llm.AdjustmentDefault {
			t.Fatalf("fitted default recorded as %+v", adjustments)
		}
		if len(prep.Warnings()) != 0 {
			t.Fatal("a default the caller never set produced a warning")
		}
	}
}

func controlsModel(tc providerCase, endpoint string) llm.ModelInfo {
	info := textModel(tc, endpoint)
	for _, f := range []llm.Feature{
		llm.FeatureStreaming,
		llm.FeatureTools,
		llm.FeatureTemperature,
		llm.FeatureReasoningEffort,
		llm.FeatureReasoningBudget,
		llm.FeatureToolChoice,
		llm.FeatureParallelTools,
	} {
		info.Capabilities.Features[f] = llm.SupportSupported
	}
	info.Capabilities.MaxOutputTokens = 8192
	info.Capabilities.ReasoningEfforts = []string{"low"}
	return info
}
