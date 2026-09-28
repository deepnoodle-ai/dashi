package responses

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestDecodeItemClassifiesOnlyRecognizedReasoning(t *testing.T) {
	wire := codec{
		origin: content.Origin{
			Provider: "openai",
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
			name: "encrypted",
			raw:  `{"type":"reasoning","encrypted_content":"private","summary":[]}`,
			kind: llm.ContentReasoning,
		},
		{
			name: "summary",
			raw:  `{"type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}`,
			kind: llm.ContentReasoning,
			text: "first\n\nsecond",
		},
		{
			name: "unknown summary part",
			raw:  `{"type":"reasoning","summary":[{"type":"future_summary","text":"private"}]}`,
			kind: llm.ContentOpaque,
		},
		{
			name: "unmodeled content",
			raw:  `{"type":"reasoning","content":[{"type":"reasoning_text","text":"private"}],"summary":[]}`,
			kind: llm.ContentOpaque,
		},
		{
			name: "no recognized payload",
			raw:  `{"type":"reasoning","future_payload":"private"}`,
			kind: llm.ContentOpaque,
		},
		{
			name: "future item",
			raw:  `{"type":"future_reasoning","encrypted_content":"private"}`,
			kind: llm.ContentOpaque,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := wire.decodeItem(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decode item: %v", err)
			}
			if len(decoded) != 1 || decoded[0].Kind != tc.kind {
				t.Fatalf("decoded = %+v, want one %q item", decoded, tc.kind)
			}
			if tc.kind != llm.ContentReasoning {
				return
			}
			if decoded[0].Reasoning.Text != tc.text || decoded[0].Replay == nil {
				t.Fatal("recognized reasoning lost display or replay")
			}
			decoded[0].Reasoning.Text = "edited display"
			encoded, err := wire.encode(decoded[0], llm.RoleAssistant)
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
