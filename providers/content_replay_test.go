package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestPrepareAdaptsPortableHistoryAndSendsRecordedBytes(t *testing.T) {
	for _, source := range cases(t) {
		_, generated := portableHistory(t, source)
		for _, target := range cases(t) {
			for _, stream := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/%s/stream=%t", source.name, target.name, stream),
					func(t *testing.T) {
						var calls atomic.Int32
						sent := make(chan []byte, 1)
						server := serve(t, func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							raw, _ := io.ReadAll(r.Body)
							sent <- raw
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								io.WriteString(w, target.stream)
							} else {
								io.WriteString(w, target.empty)
							}
						})
						client := target.new(server.URL)
						input := modelInput()
						input.Stream = stream
						input.Messages = append(input.Messages, generated.Message.Clone())
						input.Messages = append(
							input.Messages,
							toolReplies(generated.Message)...,
						)
						original := mustJSON(input)
						request, err := client.Prepare(t.Context(), input)
						if err != nil {
							t.Fatal(err)
						}
						if calls.Load() != 0 ||
							mustJSON(input) != original ||
							mustJSON(request.Input()) != original {
							t.Fatal("preparation dispatched or changed original history")
						}
						changes := request.ContentChanges()
						if len(changes) != 2 {
							t.Fatalf("missing text or tool replay decision: %+v", changes)
						}
						for i, change := range changes {
							if change.MessageIndex != 1 ||
								change.ContentIndex != i ||
								change.Action != llm.ContentChangeStripReplay ||
								change.Reason == "" {
								t.Fatalf("decision lost original position or action: %+v", change)
							}
						}
						var snapshot llm.RequestSnapshot
						if err := json.Unmarshal(
							[]byte(mustJSON(request.Snapshot())),
							&snapshot,
						); err != nil {
							t.Fatal(err)
						}
						restored, err := llm.NewRequest(snapshot)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(
							restored.Body(),
							request.Body(),
						) || !reflect.DeepEqual(
							restored.ContentChanges(),
							changes,
						) {
							t.Fatal("snapshot round trip changed body or content decisions")
						}
						// Mutation after preparation cannot change the request or source.
						input.Messages[1].Content[0].Block.Text.Text = "mutated"
						input.Messages[1].Source.ResponseID = "mutated"
						changes[0].Reason = "mutated"
						if _, err := client.Send(
							t.Context(),
							llm.Call{
								Request: restored,
							},
						); err != nil {
							t.Fatal(err)
						}
						if calls.Load() != 1 || !bytes.Equal(
							<-sent,
							request.Body(),
						) || request.ContentChanges()[0].Reason == "mutated" {
							t.Fatal("dispatch differed from immutable preparation")
						}
						body := string(request.Body())
						if !strings.Contains(
							body,
							"Hello",
						) || !strings.Contains(
							body,
							"call-1",
						) || !strings.Contains(
							body,
							"9007199254740993",
						) || strings.Contains(
							body,
							"source-item",
						) || strings.Contains(
							body,
							"mutated",
						) {
							t.Fatal("adaptation lost canonical content or retained source item metadata")
						}
						if request.Effective().MaxTokens != llm.DefaultMaxTokens ||
							len(request.Adjustments()) == 0 {
							t.Fatal("content adaptation lost setting defaults")
						}
					},
				)
			}
		}
	}
}

func TestPrepareAdaptsHistoryToAnotherModelOnSameClient(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client, generated := portableHistory(t, tc)
			input := modelInput()
			input.Model = "another-model"
			input.Messages = append(input.Messages, generated.Message)
			input.Messages = append(input.Messages, toolReplies(generated.Message)...)
			request, err := client.Prepare(t.Context(), input)
			if err != nil || len(request.ContentChanges()) != 2 {
				t.Fatalf("model change failed: %v", err)
			}
			if request.Input().Messages[1].Source.Requested.Model != "requested" {
				t.Fatal("adaptation rewrote message provenance")
			}
		})
	}
}

func TestPrepareChecksFileOriginsAtEveryContentLocation(t *testing.T) {
	for _, tc := range cases(t) {
		for _, nested := range []bool{false, true} {
			for _, variant := range []string{
				"matching",
				"model-independent",
				"different-model",
				"missing",
				"provider",
				"api",
				"endpoint",
				"credential",
			} {
				t.Run(
					fmt.Sprintf("%s/nested=%t/%s", tc.name, nested, variant),
					func(t *testing.T) {
						client := tc.new("https://unused.invalid/api")
						origin := preparedRequest(t, client).Origin()
						switch variant {
						case "model-independent":
							origin.Model = ""
						case "different-model":
							origin.Model = "other"
						case "missing":
							origin = content.Origin{}
						case "provider":
							origin.Provider = "other"
						case "api":
							origin.API = "other.api"
						case "endpoint", "credential":
							config := clientConfig{
								endpoint: "https://unused.invalid/api",
								key:      "test",
							}
							if variant == "endpoint" {
								config.endpoint += "/other"
							} else {
								config.key = "other"
							}
							origin = preparedRequest(t, configured(t, tc, config)).Origin()
						}
						input := modelInput()
						block := content.DocumentBlock(content.Source{
							ID:     "file-1",
							Origin: origin,
						})
						if nested {
							message := llm.AssistantMessage(llm.Content{
								Kind: llm.ContentToolCall,
								ToolCall: &tool.Call{
									ID:   "c",
									Name: "read",
									Args: json.RawMessage(`{}`),
								},
							})
							input.Messages = append(
								input.Messages,
								message,
								llm.UserMessage(llm.Content{
									Kind: llm.ContentToolResult,
									ToolResult: &llm.ToolResult{
										CallID: "c",
										Output: tool.Output{Content: []content.Block{block}},
									},
								}),
							)
						} else {
							input.Messages[0].Content = append(
								input.Messages[0].Content,
								llm.Block(block),
							)
						}
						request, err := client.Prepare(t.Context(), input)
						valid := variant == "matching" ||
							variant == "model-independent" ||
							variant == "different-model"
						if (err == nil) != valid {
							t.Fatalf("file acceptance=%t want=%t: %v", err == nil, valid, err)
						}
						if valid && (!strings.Contains(
							string(request.Body()),
							"file-1",
						) || strings.Contains(
							string(request.Body()),
							origin.Scope,
						)) {
							t.Fatal("file ID missing or internal scope leaked into provider payload")
						}
					},
				)
			}
		}
	}
}

func TestPrepareRejectsUnfamiliarReplayLossAndUnsafeEdits(t *testing.T) {
	for _, tc := range cases(t) {
		for _, variant := range []string{
			"unchanged",
			"edited",
			"foreign",
			"edited-citations",
			"foreign-reasoning",
			"foreign-opaque",
			"future-version",
			"missing-version",
		} {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				client := tc.new("https://unused.invalid/api")
				origin := preparedRequest(t, client).Origin()
				c := llm.Text("Hello")
				o := &llm.Opaque{
					Version: 1,
					Origin:  origin,
					Type:    "text",
					Unit:    llm.OpaqueUnitContentBlock,
					Raw:     json.RawMessage(`{"type":"text","text":"Hello","private_signature":"secret"}`),
				}
				if tc.name == "openai" {
					o.Type, o.Unit = "output_text", llm.OpaqueUnitMessagePart
					o.Raw = json.RawMessage(`{"message":{"type":"message","role":"assistant"},"part":{"type":"output_text","text":"Hello","private_signature":"secret"}}`)
				}
				c.Replay = o
				switch variant {
				case "edited":
					c.Block.Text.Text = "edited"
				case "foreign":
					o.Origin.Model = "other"
				case "edited-citations":
					c.Block.Text.Text = "edited"
					c.Block.Text.Citations = []content.Citation{
						{
							Origin: origin,
							Raw:    json.RawMessage(`{"start_index":0,"end_index":5}`),
						},
					}
				case "foreign-reasoning":
					o.Origin.Model = "other"
					c = llm.Content{
						Kind:      llm.ContentReasoning,
						Reasoning: &llm.Reasoning{Text: "visible"},
						Replay:    o,
					}
				case "foreign-opaque":
					o.Origin.Model = "other"
					c = llm.Content{Kind: llm.ContentOpaque, Opaque: o}
				case "future-version":
					o.Version = 2
				case "missing-version":
					o.Version = 0
				}
				input := modelInput()
				input.Messages = append(input.Messages, llm.AssistantMessage(c))
				request, err := client.Prepare(t.Context(), input)
				if (err == nil) != (variant == "unchanged") {
					t.Fatalf("unsafe replay acceptance for %s: %v", variant, err)
				}
				if err == nil && !strings.Contains(
					string(request.Body()),
					"private_signature",
				) {
					t.Fatal("same-origin unknown fields disappeared")
				}
			})
		}
	}
}

func TestPrepareRequiresUnambiguousCompletedToolHistory(t *testing.T) {
	for _, tc := range cases(t) {
		for _, variant := range []string{
			"complete",
			"unresolved",
			"orphan",
			"reordered",
			"duplicate-call",
			"duplicate-result",
			"unsupported-id",
		} {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				client := tc.new("https://unused.invalid/api")
				call := llm.AssistantMessage(llm.Content{
					Kind:     llm.ContentToolCall,
					ToolCall: &tool.Call{ID: "c", Name: "act", Args: json.RawMessage(`{}`)},
				})
				result := toolReplies(call)[0]
				input := modelInput()
				history := []llm.Message{call, result}
				switch variant {
				case "unresolved":
					history = []llm.Message{call}
				case "orphan":
					history = []llm.Message{result}
				case "reordered":
					history = []llm.Message{result, call}
				case "duplicate-call":
					history = []llm.Message{call, call, result}
				case "duplicate-result":
					history = append(history, result)
				case "unsupported-id":
					call.Content[0].ToolCall.ID = "needs remapping"
					result.Content[0].ToolResult.CallID = "needs remapping"
				}
				input.Messages = append(input.Messages, history...)
				_, err := client.Prepare(t.Context(), input)
				valid := variant == "complete" || variant == "unsupported-id"
				if (err == nil) != valid {
					t.Fatalf("tool pairing accepted %s: %v", variant, err)
				}
			})
		}
	}
}

func TestResultProvenanceSurvivesDetachedMessageAndPartialOutput(
	t *testing.T,
) {
	for _, tc := range cases(t) {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", tc.name, partial), func(t *testing.T) {
				server := serve(
					t,
					func(w http.ResponseWriter, r *http.Request) {
						if partial {
							w.Header().Set("Content-Type", "text/event-stream")
							io.WriteString(w, tc.partialToolStream)
						} else {
							io.WriteString(w, tc.complete)
						}
					},
				)
				client := tc.new(server.URL)
				result, err := client.Send(
					t.Context(),
					llm.Call{
						Request: preparedRequest(t, client, partial),
					},
				)
				if (err != nil) != partial {
					t.Fatalf("unexpected fixture outcome: %v", err)
				}
				var message llm.Message
				if err := json.Unmarshal(
					[]byte(mustJSON(result.Message)),
					&message,
				); err != nil {
					t.Fatal(err)
				}
				source := message.Source
				if source == nil ||
					source.Requested != result.Requested ||
					source.Served != result.Served ||
					source.ResponseID != result.ID ||
					source.API != client.API() {
					t.Fatal("detached message lost response provenance")
				}
				clone := message.Clone()
				clone.Source.ResponseID = "modified"
				if message.Source.ResponseID == "modified" {
					t.Fatal("message clone shared provenance")
				}
			})
		}
	}
}

func TestResultDoesNotInventMessageSourceWithoutOutput(t *testing.T) {
	for _, tc := range cases(t) {
		for _, variant := range []string{"cancelled", "http-error", "empty-stream"} {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				server := serve(
					t,
					func(w http.ResponseWriter, r *http.Request) {
						if variant == "http-error" {
							w.WriteHeader(http.StatusBadGateway)
						}
					},
				)
				client := tc.new(server.URL)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if variant == "cancelled" {
					cancel()
				}
				result, err := client.Send(
					ctx,
					llm.Call{
						Request: preparedRequest(t, client, variant == "empty-stream"),
					},
				)
				if err == nil ||
					result.Requested.Model != "requested" ||
					result.Message.Source != nil {
					t.Fatal(
						"failure lost attempt facts or invented provider message provenance",
						err,
					)
				}
				if variant == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPrepareRecordsRoleAndToolErrorRepresentations(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://unused.invalid/api")
			call := llm.AssistantMessage(llm.Content{
				Kind:     llm.ContentToolCall,
				ToolCall: &tool.Call{ID: "c", Name: "act", Args: json.RawMessage(`{}`)},
			})
			result := toolReplies(call)[0]
			result.Content[0].ToolResult.IsError = true
			input := modelInput()
			input.Messages = append(
				input.Messages,
				llm.Message{
					Role:    llm.RoleDeveloper,
					Content: []llm.Content{llm.Text("instruction")},
				},
				call,
				result,
			)
			request, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			changes := request.ContentChanges()
			want := llm.ContentChangeConvertRole
			if tc.name == "openai" {
				want = llm.ContentChangeEncodeToolError
			}
			if len(changes) != 1 || changes[0].Action != want {
				t.Fatalf("implicit representation change: %+v", changes)
			}
		})
	}
}

func portableHistory(t *testing.T, tc providerCase) (llm.Client, llm.Result) {
	t.Helper()
	body := `{"type":"message","id":"source-response","model":"served","stop_reason":"tool_use","content":[{"type":"text","text":"Hello"},{"type":"tool_use","id":"call-1","name":"act","input":{"counter":9007199254740993}}]}`
	if tc.name == "openai" {
		body = `{"object":"response","id":"source-response","model":"served","status":"completed","output":[{"type":"message","id":"source-item","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]},{"type":"function_call","id":"source-item-call","call_id":"call-1","name":"act","arguments":"{\"counter\":9007199254740993}","status":"completed"}]}`
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})
	client := tc.new(server.URL)
	result, err := client.Send(
		t.Context(),
		llm.Call{
			Request: preparedRequest(t, client),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return client, result
}
