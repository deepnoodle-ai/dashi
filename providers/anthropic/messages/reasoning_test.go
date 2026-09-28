package messages

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestDecodeContentClassifiesRedactedThinking(t *testing.T) {
	wire := codec{
		origin: content.Origin{
			Provider: "anthropic",
			API:      API,
			Model:    "model",
			Scope:    "scope",
		},
	}
	for _, tc := range []struct {
		name string
		raw  string
		kind llm.ContentKind
		text string
	}{
		{
			name: "signed",
			raw:  `{"type":"thinking","thinking":"display","signature":"signed"}`,
			kind: llm.ContentReasoning,
			text: "display",
		},
		{
			name: "redacted",
			raw:  `{"type":"redacted_thinking","data":"encrypted"}`,
			kind: llm.ContentReasoning,
		},
		{
			name: "missing encrypted data",
			raw:  `{"type":"redacted_thinking"}`,
			kind: llm.ContentOpaque,
		},
		{
			name: "future block",
			raw:  `{"type":"future_thinking","data":"private"}`,
			kind: llm.ContentOpaque,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := wire.decodeContent(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decode content: %v", err)
			}
			if decoded.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", decoded.Kind, tc.kind)
			}
			if tc.kind != llm.ContentReasoning {
				return
			}
			if decoded.Reasoning.Text != tc.text ||
				decoded.Replay == nil ||
				decoded.Opaque != nil {
				t.Fatal("reasoning lost its display projection or replay location")
			}
			decoded.Reasoning.Text = "edited display"
			encoded, err := wire.encode(decoded)
			if err != nil {
				t.Fatalf("replay reasoning: %v", err)
			}
			var original map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &original); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(encoded, original) {
				t.Fatalf(
					"replay changed original reasoning: got %v, want %v",
					encoded,
					original,
				)
			}
		})
	}
}
