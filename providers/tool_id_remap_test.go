package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

var remappedToolIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestPrepareRemapsToolIDsAndSendsFrozenBody(t *testing.T) {
	for _, tc := range cases(t) {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var calls atomic.Int32
				var sent []byte
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					sent, _ = io.ReadAll(r.Body)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, tc.stream)
					} else {
						io.WriteString(w, tc.empty)
					}
				})
				client := tc.new(server.URL)
				input := remapHistory("tool call: 東京 / 🚀", "valid_ID-9")
				input.Stream = stream
				original := mustJSON(input)
				request, err := client.Prepare(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatal("Prepare dispatched")
				}
				if mustJSON(input) != original {
					t.Fatal("preparation changed caller input")
				}
				if mustJSON(request.Input()) != original {
					t.Fatal("prepared request lost original execution IDs")
				}
				mapped := assertRemapPair(
					t,
					request.ContentChanges(),
					1,
					0,
					2,
					0,
					"tool call: 東京 / 🚀",
				)
				if !remappedToolIDPattern.MatchString(mapped) {
					t.Fatalf("mapped ID outside portable subset: %q", mapped)
				}
				callsOnWire, resultsOnWire := wireToolIDs(t, request.Body())
				if !reflect.DeepEqual(callsOnWire, []string{mapped, "valid_ID-9"}) {
					t.Fatalf("wire calls changed order or IDs: %q", callsOnWire)
				}
				if !reflect.DeepEqual(resultsOnWire, []string{mapped, "valid_ID-9"}) {
					t.Fatalf("wire results changed order or IDs: %q", resultsOnWire)
				}
				if bytes.Contains(request.Body(), []byte("tool call: 東京 / 🚀")) {
					t.Fatal("original unsupported ID leaked into provider body")
				}
				again, err := client.Prepare(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(again.Body(), request.Body()) {
					t.Fatal("identical preparation changed body")
				}
				if !reflect.DeepEqual(again.ContentChanges(), request.ContentChanges()) {
					t.Fatal("identical preparation changed mapping")
				}
				var snapshot llm.RequestSnapshot
				if err := json.Unmarshal([]byte(mustJSON(request.Snapshot())), &snapshot); err != nil {
					t.Fatal(err)
				}
				restored, err := llm.NewRequest(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(restored.Body(), request.Body()) {
					t.Fatal("snapshot round trip changed body")
				}
				if !reflect.DeepEqual(restored.ContentChanges(), request.ContentChanges()) {
					t.Fatal("snapshot round trip changed mapping")
				}
				if mustJSON(restored.Input()) != original {
					t.Fatal("snapshot round trip changed original input")
				}
				input.Messages[1].Content[0].ToolCall.ID = "mutated"
				input.Messages[2].Content[0].ToolResult.CallID = "mutated"
				if _, err := client.Send(context.Background(), restored, nil); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 {
					t.Fatal("Send did not dispatch exactly once")
				}
				if !bytes.Equal(sent, request.Body()) {
					t.Fatal("Send changed frozen body bytes")
				}
			})
		}
	}
}

func TestPrepareRemapIsDeterministicAcrossTransportSettingsAndScope(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			var mapped []string
			for _, variant := range []string{"base", "stream", "settings", "credential", "endpoint"} {
				config := clientConfig{
					endpoint: "https://unused.invalid/api",
					key:      "key-one",
					unknown:  llm.UnknownAllow,
				}
				if variant == "credential" {
					config.key = "key-two"
				}
				if variant == "endpoint" {
					config.endpoint += "/other"
				}
				client := configured(t, tc, config)
				input := remapHistory(strings.Repeat("x", 65))
				if variant == "stream" {
					input.Stream = true
				}
				if variant == "settings" {
					input.Settings.MaxTokens = 40
				}
				request, err := client.Prepare(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				mapped = append(mapped, assertRemapPair(
					t,
					request.ContentChanges(),
					1,
					0,
					2,
					0,
					strings.Repeat("x", 65),
				))
			}
			for _, id := range mapped[1:] {
				if id != mapped[0] {
					t.Fatalf("mapping changed with transport, settings, or scope: %q", mapped)
				}
			}
		})
	}
}

func TestPrepareReservesSupportedToolIDsBeforeAllocating(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://unused.invalid/api")
			original := "needs remapping"
			first, err := client.Prepare(context.Background(), remapHistory(original))
			if err != nil {
				t.Fatal(err)
			}
			candidate := assertRemapPair(t, first.ContentChanges(), 1, 0, 2, 0, original)
			input := remapHistory(original, candidate)
			request, err := client.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			mapped := assertRemapPair(t, request.ContentChanges(), 1, 0, 2, 0, original)
			if mapped == candidate {
				t.Fatal("generated ID collided with a later supported ID")
			}
			calls, results := wireToolIDs(t, request.Body())
			if !reflect.DeepEqual(calls, []string{mapped, candidate}) {
				t.Fatalf("reservation changed call order: %q", calls)
			}
			if !reflect.DeepEqual(results, []string{mapped, candidate}) {
				t.Fatalf("reservation changed result pairing: %q", results)
			}
		})
	}
}

func TestPrepareRemapsParallelCallsWithoutReordering(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			ids := []string{"punctuation !", "東京", "valid-3", strings.Repeat("L", 65)}
			input := modelInput()
			calls := make([]llm.Content, 0, len(ids))
			results := make([]llm.Content, 0, len(ids))
			for _, id := range ids {
				calls = append(calls, remapCall(id))
				results = append(results, remapResult(id))
			}
			input.Messages = append(
				input.Messages,
				llm.AssistantMessage(calls...),
				llm.UserMessage(results...),
			)
			request, err := tc.new("https://unused.invalid/api").Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(ids)
			for i, id := range ids {
				if id == "valid-3" {
					continue
				}
				want[i] = assertRemapPair(t, request.ContentChanges(), 1, i, 2, i, id)
			}
			wireCalls, wireResults := wireToolIDs(t, request.Body())
			if !reflect.DeepEqual(wireCalls, want) {
				t.Fatalf("parallel call order changed: got=%q want=%q", wireCalls, want)
			}
			if !reflect.DeepEqual(wireResults, want) {
				t.Fatalf("parallel result pairing changed: got=%q want=%q", wireResults, want)
			}
			if len(request.ContentChanges()) != 6 {
				t.Fatalf("unexpected decisions for supported ID: %+v", request.ContentChanges())
			}
		})
	}
}

func TestPrepareRejectsInvalidToolHistoryEvenWithRemapping(t *testing.T) {
	for _, tc := range cases(t) {
		for _, variant := range []string{
			"empty-call", "empty-result", "duplicate-call", "duplicate-result",
			"orphan", "unresolved", "result-before-call",
		} {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				call := llm.AssistantMessage(remapCall("invalid id"))
				result := llm.UserMessage(remapResult("invalid id"))
				history := []llm.Message{call, result}
				switch variant {
				case "empty-call":
					history[0].Content[0].ToolCall.ID = ""
				case "empty-result":
					history[1].Content[0].ToolResult.CallID = ""
				case "duplicate-call":
					history = []llm.Message{call, call, result}
				case "duplicate-result":
					history = append(history, result)
				case "orphan":
					history = []llm.Message{result}
				case "unresolved":
					history = []llm.Message{call}
				case "result-before-call":
					history = []llm.Message{result, call}
				}
				input := modelInput()
				input.Messages = append(input.Messages, history...)
				client := tc.new("https://unused.invalid/api")
				if _, err := client.Prepare(context.Background(), input); err == nil {
					t.Fatalf("accepted %s", variant)
				}
			})
		}
	}
}

func TestPrepareRemapKeepsOriginalIndicesAfterReasoningOmission(t *testing.T) {
	for _, source := range cases(t) {
		_, generated := generatedReasoningHistory(t, source)
		for _, target := range cases(t) {
			if source.name == target.name {
				continue
			}
			t.Run(source.name+"-to-"+target.name, func(t *testing.T) {
				message := generated.Message.Clone()
				var callIndex int
				for i := range message.Content {
					if message.Content[i].ToolCall != nil {
						callIndex = i
						message.Content[i].ToolCall.ID = "reasoning / call"
					}
				}
				input := modelInput()
				input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
				input.Messages = append(
					input.Messages,
					message,
					llm.UserMessage(remapResult("reasoning / call")),
				)
				client := target.new("https://unused.invalid/api")
				request, err := client.Prepare(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				mapped := assertRemapPair(
					t,
					request.ContentChanges(),
					1,
					callIndex,
					2,
					0,
					"reasoning / call",
				)
				for i, c := range message.Content {
					if c.Kind == llm.ContentReasoning {
						assertReasoningDecision(t, request.ContentChanges(), 1, i)
					}
				}
				calls, results := wireToolIDs(t, request.Body())
				if !reflect.DeepEqual(calls, []string{mapped}) {
					t.Fatalf("omission changed call ID: %q", calls)
				}
				if !reflect.DeepEqual(results, []string{mapped}) {
					t.Fatalf("omission changed result ID: %q", results)
				}
			})
		}
	}
}

func TestPrepareRemapOverlaysKnownReplayWithoutChangingIndependentItemID(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://unused.invalid/api")
			origin := preparedRequest(t, client).Origin()
			input := remapHistory("replay / id")
			call := &input.Messages[1].Content[0]
			call.Replay = remapCallReplay(tc.name, origin, "replay / id", "")
			request, err := client.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			mapped := assertRemapPair(t, request.ContentChanges(), 1, 0, 2, 0, "replay / id")
			if tc.name == "openai" {
				if !bytes.Contains(request.Body(), []byte(`"id":"independent-item"`)) {
					t.Fatal("Responses item ID was changed with call_id")
				}
			}
			calls, results := wireToolIDs(t, request.Body())
			if !reflect.DeepEqual(calls, []string{mapped}) {
				t.Fatalf("known replay did not overlay mapped call ID: %q", calls)
			}
			if !reflect.DeepEqual(results, []string{mapped}) {
				t.Fatalf("result did not use mapped call ID: %q", results)
			}
			call.Replay = remapCallReplay(tc.name, origin, "replay / id", "replay / id")
			if _, err := client.Prepare(context.Background(), input); err == nil {
				t.Fatal("remap silently retained unknown private dependency on old ID")
			}
		})
	}
}

func TestPrepareRemapLeavesCompatibleUnknownOpaqueContentOpaque(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://unused.invalid/api")
			origin := preparedRequest(t, client).Origin()
			unit := llm.OpaqueUnitContentBlock
			if tc.name == "openai" {
				unit = llm.OpaqueUnitItem
			}
			original := "opaque / call"
			input := remapHistory(original)
			input.Messages[1].Content = append(
				[]llm.Content{
					{
						Kind: llm.ContentOpaque,
						Opaque: &llm.Opaque{
							Version: 1,
							Origin:  origin,
							Type:    "future_private",
							Unit:    unit,
							Raw:     json.RawMessage(`{"type":"future_private","secret":"opaque-data"}`),
						},
					},
				},
				input.Messages[1].Content...,
			)
			request, err := client.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			mapped := assertRemapPair(t, request.ContentChanges(), 1, 1, 2, 0, original)
			if len(request.ContentChanges()) != 2 {
				t.Fatalf(
					"unknown opaque content was treated as a tool call: %+v",
					request.ContentChanges(),
				)
			}
			if !bytes.Contains(request.Body(), []byte(`"secret":"opaque-data"`)) {
				t.Fatal("compatible opaque content was lost")
			}
			calls, results := wireToolIDs(t, request.Body())
			if !reflect.DeepEqual(calls, []string{mapped}) {
				t.Fatalf("opaque content changed call: %q", calls)
			}
			if !reflect.DeepEqual(results, []string{mapped}) {
				t.Fatalf("opaque content changed result: %q", results)
			}
		})
	}
}

func remapHistory(ids ...string) llm.Input {
	input := modelInput()
	for _, id := range ids {
		input.Messages = append(
			input.Messages,
			llm.AssistantMessage(remapCall(id)),
			llm.UserMessage(remapResult(id)),
		)
	}
	return input
}

func remapCall(id string) llm.Content {
	return llm.Content{
		Kind: llm.ContentToolCall,
		ToolCall: &tool.Call{
			ID:   id,
			Name: "act",
			Args: json.RawMessage(`{}`),
		},
	}
}

func remapResult(id string) llm.Content {
	return llm.Content{
		Kind: llm.ContentToolResult,
		ToolResult: &llm.ToolResult{
			CallID: id,
			Output: tool.Output{},
		},
	}
}

func assertRemapPair(
	t *testing.T,
	changes []llm.ContentChange,
	callMessage int,
	callContent int,
	resultMessage int,
	resultContent int,
	original string,
) string {
	t.Helper()
	var mapped string
	for _, position := range [][2]int{
		{callMessage, callContent},
		{resultMessage, resultContent},
	} {
		var found int
		for _, change := range changes {
			if change.MessageIndex != position[0] ||
				change.ContentIndex != position[1] ||
				change.Action != llm.ContentChangeRemapToolID {
				continue
			}
			found++
			if change.FromID != original || change.ToID == "" ||
				change.ToID == original || change.Reason == "" {
				t.Fatalf("incomplete remap decision: %+v", change)
			}
			if mapped != "" && mapped != change.ToID {
				t.Fatalf("call and result mapped differently: %+v", changes)
			}
			mapped = change.ToID
		}
		if found != 1 {
			t.Fatalf("want one remap decision at %v, got %d: %+v", position, found, changes)
		}
	}
	return mapped
}

func wireToolIDs(t *testing.T, body []byte) (calls, results []string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			switch value["type"] {
			case "tool_use":
				calls = append(calls, value["id"].(string))
			case "function_call":
				calls = append(calls, value["call_id"].(string))
			case "tool_result":
				results = append(results, value["tool_use_id"].(string))
			case "function_call_output":
				results = append(results, value["call_id"].(string))
			}
			for _, nested := range value {
				walk(nested)
			}
		case []any:
			for _, nested := range value {
				walk(nested)
			}
		}
	}
	walk(decoded)
	return calls, results
}

func remapCallReplay(name string, origin content.Origin, id, private string) *llm.Opaque {
	privateField := ""
	if private != "" {
		privateField = fmt.Sprintf(`,"related_call_id":%q`, private)
	}
	if name == "openai" {
		return &llm.Opaque{
			Version: 1,
			Origin:  origin,
			Type:    "function_call",
			Unit:    llm.OpaqueUnitItem,
			Raw: json.RawMessage(fmt.Sprintf(
				`{"type":"function_call","id":"independent-item","call_id":%q,"name":"act","arguments":"{}"%s}`,
				id,
				privateField,
			)),
		}
	}
	return &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    "tool_use",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw: json.RawMessage(fmt.Sprintf(
			`{"type":"tool_use","id":%q,"name":"act","input":{}%s}`,
			id,
			privateField,
		)),
	}
}
