package providers_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

// This synthesized Chat Completions response tests an offline handoff. It
// does not qualify live model acceptance at either destination.
const dialectHandoffResponse = `{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-a","type":"function","function":{"name":"search","arguments":"{\"q\":\"yes\"}"}},{"id":"call-b","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}]}`

func TestCompletionsTwoToolHistoryPreparesForOtherRoutes(t *testing.T) {
	sourceServer := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, dialectHandoffResponse)
	})
	source, err := completions.New(completions.Config{
		Provider:         "openai",
		APIKey:           "test",
		Endpoint:         sourceServer.URL,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := source.Send(
		t.Context(),
		llm.Call{Request: preparedRequest(t, source)},
	)
	if err != nil {
		t.Fatal(err)
	}
	replies := toolReplies(generated.Message)
	if generated.Message.Source == nil ||
		generated.Message.Source.API != completions.API ||
		len(replies) != 1 || len(replies[0].Content) != 2 {
		t.Fatalf("source result lacks two correlated calls: %+v", generated)
	}
	replies[0].Content[0].ToolResult.Output.Content[0] = content.TextBlock("search-hit")
	replies[0].Content[1].ToolResult.Output.Content[0] = content.TextBlock("lookup-row")
	input := modelInput()
	input.Messages = append(input.Messages, generated.Message)
	input.Messages = append(input.Messages, replies...)
	original := mustJSON(input)

	for _, target := range cases(t) {
		t.Run("completions/two-tool-handoff/"+target.name, func(t *testing.T) {
			var dispatches atomic.Int32
			sent := make(chan []byte, 1)
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				dispatches.Add(1)
				dispatched, _ := io.ReadAll(r.Body)
				sent <- dispatched
				_, _ = io.WriteString(w, target.empty)
			})
			client := target.new(server.URL)
			request, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if dispatches.Load() != 0 ||
				mustJSON(input) != original ||
				mustJSON(request.Input()) != original {
				t.Fatal("Prepare dispatched or changed the saved history")
			}
			for _, change := range request.ContentChanges() {
				if change.MessageIndex < 1 || change.MessageIndex > 2 ||
					change.Reason == "" {
					t.Fatalf("change lacks original history position: %+v", change)
				}
			}
			body := request.Body()
			for _, fragment := range []string{
				"call-a", "call-b", "search", "lookup", "yes",
				"search-hit", "lookup-row",
			} {
				if !strings.Contains(string(body), fragment) {
					t.Fatalf("prepared body lost %q: %s", fragment, body)
				}
			}
			results := handoffWireResults(t, body)
			if !strings.Contains(results["call-a"], "search-hit") ||
				!strings.Contains(results["call-b"], "lookup-row") ||
				len(results) != 2 {
				t.Fatalf("tool results lost their call pairing: %+v", results)
			}
			restored, err := llm.NewRequest(request.Snapshot())
			if err != nil || !bytes.Equal(restored.Body(), body) {
				t.Fatalf("snapshot changed prepared bytes: %v", err)
			}
			if _, err := client.Send(t.Context(), llm.Call{Request: restored}); err != nil {
				t.Fatal(err)
			}
			if dispatches.Load() != 1 || !bytes.Equal(<-sent, body) {
				t.Fatal("Send did not dispatch the frozen body exactly once")
			}
		})
	}
}

func handoffWireResults(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			var id string
			switch value["type"] {
			case "tool_result":
				id, _ = value["tool_use_id"].(string)
			case "function_call_output":
				id, _ = value["call_id"].(string)
			}
			if id != "" {
				encoded, _ := json.Marshal(value)
				results[id] = string(encoded)
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
	return results
}
