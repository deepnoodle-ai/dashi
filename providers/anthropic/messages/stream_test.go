package messages

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Anthropic can stream empty argument fragments for a tool with no arguments.
func TestStreamToolCallWithNoArguments(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		want      string
	}{
		{name: "empty fragments", fragments: []string{"", ""}, want: `{}`},
		{name: "mixed fragments", fragments: []string{"", `{"n":`, "", `1}`}, want: `{"n":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{
				`{"type":"message_start","message":{"id":"m","type":"message","model":"fixture","content":[]}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"act","input":{}}}`,
			}
			for _, fragment := range tc.fragments {
				encoded, _ := json.Marshal(fragment)
				events = append(events, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(encoded)+`}}`)
			}
			events = append(events,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
				`{"type":"message_stop"}`,
			)
			stream := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
			wire := codec{origin: content.Origin{Provider: "anthropic", API: API, Model: "fixture", Scope: "fixture"}}
			result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Stop.Kind != llm.StopToolUse || len(result.Message.Content) != 1 {
				t.Fatalf("unexpected result: %+v", result)
			}
			call := result.Message.Content[0].ToolCall
			if call == nil || string(call.Args) != tc.want {
				t.Fatalf("tool call = %+v, want args %s", call, tc.want)
			}
		})
	}
}
