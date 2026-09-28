package completions

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestPrepareRemapsParallelToolIDsAndSendsFrozenBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "complete"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			var dispatched []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				dispatched, _ = io.ReadAll(r.Body)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(
						w,
						"data: "+`{"id":"chat-1","model":"served","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n",
					)
					io.WriteString(w, "data: [DONE]\n\n")
				} else {
					io.WriteString(
						w,
						`{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
					)
				}
			}))
			defer server.Close()

			client := testClient(t, server.URL)
			input := remapCompletionsInput(stream)
			original := completionsJSON(t, input)
			request, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 0 {
				t.Fatal("Prepare dispatched before Send")
			}
			if completionsJSON(t, input) != original {
				t.Fatal("preparation changed caller input")
			}
			if completionsJSON(t, request.Input()) != original {
				t.Fatal("prepared request lost original execution IDs")
			}

			changes := request.ContentChanges()
			if len(changes) != 4 {
				t.Fatalf("want call and result records for both IDs: %+v", changes)
			}
			first := completionsMappedID(t, changes, 1, 0, "needs mapping")
			second := completionsMappedID(t, changes, 1, 1, "工具/second")
			if first == second {
				t.Fatal("distinct calls mapped to the same ID")
			}
			if completionsMappedID(t, changes, 2, 0, "工具/second") != second {
				t.Fatal("first result lost its second call pairing")
			}
			if completionsMappedID(t, changes, 2, 1, "needs mapping") != first {
				t.Fatal("second result lost its first call pairing")
			}

			messages := completionsWireMessages(t, request.Body())
			if len(messages) != 5 {
				t.Fatalf(
					"want system, user, assistant, and two tool messages: %+v",
					messages,
				)
			}
			assistant := messages[2]
			if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 {
				t.Fatalf("parallel calls missing from assistant message: %+v", assistant)
			}
			if assistant.ToolCalls[0].ID != first {
				t.Fatalf("first call ID = %q, want %q", assistant.ToolCalls[0].ID, first)
			}
			if assistant.ToolCalls[1].ID != second {
				t.Fatalf(
					"second call ID = %q, want %q",
					assistant.ToolCalls[1].ID,
					second,
				)
			}
			var args map[string]string
			if err := json.Unmarshal(
				[]byte(assistant.ToolCalls[0].Function.Arguments),
				&args,
			); err != nil {
				t.Fatal(err)
			}
			if args["reference"] != "needs mapping" {
				t.Fatal("tool argument containing an old ID was rewritten")
			}
			if messages[3].ToolCallID != second {
				t.Fatalf("first result ID = %q, want %q", messages[3].ToolCallID, second)
			}
			if messages[4].ToolCallID != first {
				t.Fatalf("second result ID = %q, want %q", messages[4].ToolCallID, first)
			}
			if messages[3].Content != "finished 工具/second" {
				t.Fatalf("first result text changed: %q", messages[3].Content)
			}
			if messages[4].Content != "finished needs mapping" {
				t.Fatalf("second result text changed: %q", messages[4].Content)
			}
			again, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again.Body(), request.Body()) {
				t.Fatal("repeated preparation changed body")
			}
			if !reflect.DeepEqual(again.ContentChanges(), changes) {
				t.Fatal("repeated preparation changed records")
			}
			var snapshot llm.RequestSnapshot
			if err := json.Unmarshal(
				[]byte(completionsJSON(t, request.Snapshot())),
				&snapshot,
			); err != nil {
				t.Fatal(err)
			}
			restored, err := llm.NewRequest(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored.Body(), request.Body()) {
				t.Fatal("snapshot round trip changed body")
			}
			if !reflect.DeepEqual(restored.ContentChanges(), changes) {
				t.Fatal("snapshot round trip changed mapping")
			}
			if completionsJSON(t, restored.Input()) != original {
				t.Fatal("snapshot round trip changed original input")
			}
			input.Messages[1].Content[0].ToolCall.ID = "mutated"
			input.Messages[2].Content[1].ToolResult.CallID = "mutated"
			changes[0].ToID = "mutated"
			if _, err := client.Send(
				t.Context(),
				llm.Call{
					Request: restored,
				},
			); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 1 {
				t.Fatal("Send did not dispatch exactly once")
			}
			if !bytes.Equal(dispatched, request.Body()) {
				t.Fatal("Send changed frozen body bytes")
			}
			if strings.Contains(string(dispatched), `"id":"needs mapping"`) {
				t.Fatal("old ID remained in a tool call ID field")
			}
			if strings.Contains(string(dispatched), `"tool_call_id":"needs mapping"`) {
				t.Fatal("old ID remained in a tool result ID field")
			}
		})
	}
}

func remapCompletionsInput(stream bool) llm.Input {
	input := testInput(stream)
	input.Messages = append(
		input.Messages,
		llm.AssistantMessage(
			llm.Content{
				Kind: llm.ContentToolCall,
				ToolCall: &tool.Call{
					ID:   "needs mapping",
					Name: "search",
					Args: json.RawMessage(`{"reference":"needs mapping"}`),
				},
			},
			llm.Content{
				Kind: llm.ContentToolCall,
				ToolCall: &tool.Call{
					ID:   "工具/second",
					Name: "search",
					Args: json.RawMessage(`{"reference":"工具/second"}`),
				},
			},
		),
		llm.UserMessage(
			llm.Content{
				Kind: llm.ContentToolResult,
				ToolResult: &llm.ToolResult{
					CallID: "工具/second",
					Output: tool.Output{
						Content: []content.Block{content.TextBlock("finished 工具/second")},
					},
				},
			},
			llm.Content{
				Kind: llm.ContentToolResult,
				ToolResult: &llm.ToolResult{
					CallID: "needs mapping",
					Output: tool.Output{
						Content: []content.Block{content.TextBlock("finished needs mapping")},
					},
				},
			},
		),
	)
	return input
}

func completionsMappedID(
	t *testing.T,
	changes []llm.ContentChange,
	message int,
	content int,
	original string,
) string {
	t.Helper()
	for _, change := range changes {
		if change.MessageIndex == message && change.ContentIndex == content &&
			change.Action == llm.ContentChangeRemapToolID &&
			change.FromID == original && change.ToID != "" &&
			change.ToID != original && change.Reason != "" {
			return change.ToID
		}
	}
	t.Fatalf(
		"missing remap record at message %d content %d: %+v",
		message,
		content,
		changes,
	)
	return ""
}

func completionsWireMessages(t *testing.T, body []byte) []Message {
	t.Helper()
	var payload struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Messages
}

func completionsJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
