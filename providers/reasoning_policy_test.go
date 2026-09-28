package providers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestPrepareOmitsOnlyIncompatibleKnownReasoning(t *testing.T) {
	for _, source := range cases(t) {
		sourceClient, generated := generatedReasoningHistory(t, source)
		var reasoningIndices []int
		for i, c := range generated.Message.Content {
			if c.Kind == llm.ContentReasoning {
				reasoningIndices = append(reasoningIndices, i)
			}
		}
		if len(reasoningIndices) == 0 {
			t.Fatalf("%s fixture produced no known reasoning", source.name)
		}
		for _, target := range cases(t) {
			if target.name == source.name {
				continue
			}
			for _, stream := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s-to-%s/stream=%t", source.name, target.name, stream),
					func(t *testing.T) {
						var calls atomic.Int32
						sent := make(chan []byte, 1)
						server := serve(t, func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							body, _ := io.ReadAll(r.Body)
							sent <- body
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
						if _, err := client.Prepare(t.Context(), input); err == nil {
							t.Fatal("default policy accepted incompatible reasoning")
						}
						if calls.Load() != 0 || mustJSON(input) != original {
							t.Fatal("strict preparation dispatched or changed original history")
						}
						input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
						original = mustJSON(input)
						request, err := client.Prepare(t.Context(), input)
						if err != nil {
							t.Fatalf("explicit reasoning omission failed: %v", err)
						}
						if calls.Load() != 0 ||
							mustJSON(input) != original ||
							mustJSON(request.Input()) != original {
							t.Fatal("preparation dispatched or changed original history")
						}
						changes := request.ContentChanges()
						for _, index := range reasoningIndices {
							assertReasoningDecision(t, changes, 1, index)
						}
						for i, c := range generated.Message.Content {
							if c.Kind == llm.ContentBlock || c.Kind == llm.ContentToolCall {
								assertContentDecision(t, changes, 1, i, llm.ContentChangeStripReplay)
							}
						}
						if len(changes) != len(generated.Message.Content) {
							t.Fatalf("unexplained adaptation decisions: %+v", changes)
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
							"done",
						) ||
							strings.Contains(
								body,
								"private-thought",
							) || strings.Contains(
							body,
							"signed-secret",
						) ||
							strings.Contains(
								body,
								"redacted-secret",
							) || strings.Contains(
							body,
							"encrypted-secret",
						) {
							t.Fatalf(
								"prepared body lost portable history or leaked reasoning: %s",
								body,
							)
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
						if restored.Input().ContentPolicy.UnreplayableReasoning != llm.ReasoningOmit ||
							!reflect.DeepEqual(
								restored.ContentChanges(),
								changes,
							) || !bytes.Equal(
							restored.Body(),
							request.Body(),
						) {
							t.Fatal("snapshot lost omission policy, decisions, or exact body")
						}
						input.Messages[1].Content[reasoningIndices[0]].Reasoning.Text = "mutated"
						changes[0].Reason = "mutated"
						if _, err := client.Send(
							t.Context(),
							llm.Call{
								Request: restored,
							},
						); err != nil {
							t.Fatal(err)
						}
						if calls.Load() != 1 || !bytes.Equal(<-sent, request.Body()) ||
							mustJSON(request.Input()) != original ||
							request.ContentChanges()[0].Reason == "mutated" {
							t.Fatal("dispatch differed from frozen preparation")
						}
					},
				)
			}
		}
		// An opt-in policy must leave compatible private reasoning intact.
		input := modelInput()
		input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
		input.Messages = append(input.Messages, generated.Message)
		input.Messages = append(input.Messages, toolReplies(generated.Message)...)
		request, err := sourceClient.Prepare(t.Context(), input)
		if err != nil {
			t.Fatalf("%s same-origin reasoning rejected: %v", source.name, err)
		}
		if len(request.ContentChanges()) != 0 {
			t.Fatalf("compatible reasoning was changed: %+v", request.ContentChanges())
		}
		body := string(request.Body())
		if source.name == "anthropic" && (!strings.Contains(
			body,
			"signed-secret",
		) || !strings.Contains(
			body,
			"redacted-secret",
		)) {
			t.Fatal("compatible Anthropic reasoning lost private replay")
		}
		if source.name == "openai" && !strings.Contains(body, "encrypted-secret") {
			t.Fatal("compatible Responses reasoning lost private replay")
		}
	}
}

func TestPrepareOmitsReasoningOnlyMessagesWithoutMovingHistory(t *testing.T) {
	for _, source := range cases(t) {
		_, generated := generatedReasoningHistory(t, source)
		for _, target := range cases(t) {
			if target.name == source.name {
				continue
			}
			t.Run(source.name+"-to-"+target.name, func(t *testing.T) {
				client := target.new("https://unused.invalid/api")
				input := modelInput()
				input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
				var reasoning []llm.Content
				var portable []llm.Content
				for _, c := range generated.Message.Content {
					if c.Kind == llm.ContentReasoning {
						reasoning = append(reasoning, c)
					} else {
						portable = append(portable, c)
					}
				}
				input.Messages = append(
					input.Messages,
					llm.Message{
						Role:    llm.RoleAssistant,
						Content: reasoning,
						Source:  generated.Message.Source,
					},
					llm.Message{
						Role:    llm.RoleAssistant,
						Content: portable,
						Source:  generated.Message.Source,
					},
				)
				input.Messages = append(input.Messages, toolReplies(input.Messages[2])...)
				input.Messages = append(
					input.Messages,
					llm.UserMessage(llm.Text("after tools")),
				)
				request, err := client.Prepare(t.Context(), input)
				if err != nil {
					t.Fatalf("history after reasoning-only message rejected: %v", err)
				}
				for i := range reasoning {
					assertReasoningDecision(t, request.ContentChanges(), 1, i)
				}
				for i := range portable {
					assertContentDecision(
						t,
						request.ContentChanges(),
						2,
						i,
						llm.ContentChangeStripReplay,
					)
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
					"done",
				) || !strings.Contains(
					body,
					"after tools",
				) {
					t.Fatalf("omission moved or broke following history: %s", body)
				}
				input.Messages = []llm.Message{
					{Role: llm.RoleAssistant, Content: reasoning},
				}
				if _, err := client.Prepare(t.Context(), input); err == nil {
					t.Fatal("entirely omitted history was accepted")
				}
			})
		}
	}
}

func TestReasoningOmissionDoesNotBypassReplayGuards(t *testing.T) {
	for _, source := range cases(t) {
		_, generated := generatedReasoningHistory(t, source)
		var known llm.Content
		for _, c := range generated.Message.Content {
			if c.Kind == llm.ContentReasoning {
				known = c
				break
			}
		}
		for _, target := range cases(t) {
			if target.name == source.name {
				continue
			}
			client := target.new("https://unused.invalid/api")
			for _, variant := range []string{
				"unknown-opaque",
				"foreign-citation",
				"future-wrapper",
				"missing-wrapper-version",
				"partial-wrapper",
				"wrong-unit",
				"wrong-type",
				"wrong-raw-shape",
			} {
				t.Run(source.name+"-to-"+target.name+"/"+variant, func(t *testing.T) {
					c := known.Clone()
					switch variant {
					case "unknown-opaque":
						c = llm.Content{
							Kind: llm.ContentOpaque,
							Opaque: &llm.Opaque{
								Version: 1,
								Origin:  known.Replay.Origin,
								Unit:    known.Replay.Unit,
								Type:    "future_private",
								Raw:     json.RawMessage(`{"type":"future_private","secret":"opaque"}`),
							},
						}
					case "foreign-citation":
						c = llm.Text("cited")
						c.Block.Text.Citations = []content.Citation{
							{
								Origin: known.Replay.Origin,
								Raw:    json.RawMessage(`{"type":"url_citation","start_index":0,"end_index":5}`),
							},
						}
					case "future-wrapper":
						c.Replay.Version = 2
					case "missing-wrapper-version":
						c.Replay.Version = 0
					case "partial-wrapper":
						c.Replay.Partial = true
					case "wrong-unit":
						c.Replay.Unit = llm.OpaqueUnitMessagePart
						if source.name == "openai" {
							c.Replay.Unit = llm.OpaqueUnitContentBlock
						}
					case "wrong-type":
						c.Replay.Type = "text"
					case "wrong-raw-shape":
						c.Replay.Raw = json.RawMessage(`{"type":"text","text":"not reasoning"}`)
					}
					input := modelInput()
					input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
					input.Messages = append(input.Messages, llm.AssistantMessage(c))
					if _, err := client.Prepare(t.Context(), input); err == nil {
						t.Fatalf("reasoning omission accepted %s", variant)
					}
				})
			}
		}
	}
}

func generatedReasoningHistory(
	t *testing.T,
	tc providerCase,
) (llm.Client, llm.Result) {
	t.Helper()
	fixture := `{"type":"message","id":"reasoning-source","model":"requested","stop_reason":"tool_use","content":[` +
		`{"type":"thinking","thinking":"private-thought","signature":"signed-secret"},` +
		`{"type":"text","text":"Hello"},` +
		`{"type":"redacted_thinking","data":"redacted-secret"},` +
		`{"type":"tool_use","id":"call-1","name":"act","input":{}}]}`
	if tc.name == "openai" {
		fixture = `{"object":"response","id":"reasoning-source","model":"requested","status":"completed","output":[` +
			`{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"private-thought"}],"encrypted_content":"encrypted-secret"},` +
			`{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]},` +
			`{"type":"function_call","id":"fc","call_id":"call-1","name":"act","arguments":"{}","status":"completed"}]}`
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fixture)
	})
	client := tc.new(server.URL)
	result, err := client.Send(
		t.Context(),
		llm.Call{
			Request: preparedRequest(t, client),
		},
	)
	if err != nil {
		t.Fatalf("decode %s reasoning fixture: %v", tc.name, err)
	}
	return client, result
}

func assertReasoningDecision(
	t *testing.T,
	changes []llm.ContentChange,
	message, content int,
) {
	t.Helper()
	assertContentDecision(
		t,
		changes,
		message,
		content,
		llm.ContentChangeOmitReasoning,
	)
}

func assertContentDecision(
	t *testing.T,
	changes []llm.ContentChange,
	message, content int,
	action llm.ContentChangeAction,
) {
	t.Helper()
	for _, change := range changes {
		if change.MessageIndex == message &&
			change.ContentIndex == content &&
			change.Action == action &&
			change.Reason != "" {
			return
		}
	}
	t.Fatalf(
		"missing %s decision at original message %d content %d: %+v",
		action,
		message,
		content,
		changes,
	)
}
