package providercontent

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestAdaptGeminiSignedReplayRequiresUnchangedID(t *testing.T) {
	origin := content.Origin{
		Provider: "google",
		API:      "google.generate_content",
		Model:    "gemini",
	}
	for _, tc := range []struct {
		name string
		id   string
		raw  string
		fail bool
	}{
		{
			name: "unchanged signed",
			id:   "call-1",
			raw:  `{"functionCall":{"id":"call-1","name":"lookup","args":{}},"thoughtSignature":"signed"}`,
		},
		{
			name: "unsigned remap",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"call.with.dot","name":"lookup","args":{}}}`,
		},
		{
			name: "signed missing ID preserves raw replay",
			id:   "call-1",
			raw:  `{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"signed"}`,
		},
		{
			name: "unsigned missing ID permits injection",
			id:   "call-1",
			raw:  `{"functionCall":{"name":"lookup","args":{}}}`,
		},
		{
			name: "signed empty ID preserves raw replay",
			id:   "call-1",
			raw:  `{"functionCall":{"id":"","name":"lookup","args":{}},"thoughtSignature":"signed"}`,
		},
		{
			name: "unsigned remap with missing ID",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"name":"lookup","args":{}}}`,
		},
		{
			name: "signed remap",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"call.with.dot","name":"lookup","args":{}},"thoughtSignature":"signed"}`,
			fail: true,
		},
		{
			name: "signed missing ID cannot be remapped",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"signed"}`,
			fail: true,
		},
		{
			name: "foreign explicit source ID",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"another.id","name":"lookup","args":{}}}`,
			fail: true,
		},
		{
			name: "unknown outer field on remap",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"call.with.dot","name":"lookup","args":{}},"private":true}`,
			fail: true,
		},
		{
			name: "unknown nested field on remap",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"call.with.dot","name":"lookup","args":{},"private":true}}`,
			fail: true,
		},
		{
			name: "duplicate ID on remap",
			id:   "call.with.dot",
			raw:  `{"functionCall":{"id":"other.id","id":"call.with.dot","name":"lookup","args":{}}}`,
			fail: true,
		},
		{
			name: "missing arguments",
			id:   "call-1",
			raw:  `{"functionCall":{"id":"call-1","name":"lookup"}}`,
			fail: true,
		},
		{
			name: "null functionCall",
			id:   "call-1",
			raw:  `{"functionCall":null}`,
			fail: true,
		},
		{
			name: "null ID",
			id:   "call-1",
			raw:  `{"functionCall":{"id":null,"name":"lookup","args":{}}}`,
			fail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := geminiToolReplay(
				tc.id,
				tc.raw,
				origin,
			)
			out, _, err := Adapt(input, origin)
			if (err != nil) != tc.fail {
				t.Fatalf(
					"adaptation error = %v, want failure %v",
					err,
					tc.fail,
				)
			}
			if err == nil && tc.id == "call.with.dot" &&
				out.Messages[0].Content[0].ToolCall.ID == tc.id {
				t.Fatal("safe unsigned call ID was not remapped")
			}
		})
	}
}

func TestAdaptGeminiTextPreservesUnchangedPrivateReplay(t *testing.T) {
	origin := content.Origin{
		Provider: "google",
		API:      "google.generate_content",
		Model:    "gemini",
	}
	text := llm.Text("hello")
	text.Replay = &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    "text",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"text":"hello","thoughtSignature":"signed","private":"keep"}`),
	}
	input := llm.Input{Messages: []llm.Message{llm.AssistantMessage(text)}}
	out, changes, err := Adapt(input, origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 ||
		string(out.Messages[0].Content[0].Replay.Raw) != string(text.Replay.Raw) {
		t.Fatal("unchanged private text replay was altered")
	}
	text.Block.Text.Text = "edited"
	if _, _, err := Adapt(
		llm.Input{
			Messages: []llm.Message{llm.AssistantMessage(text)},
		},
		origin,
	); err == nil {
		t.Fatal("edited Gemini text replay was accepted")
	}
	text.Block.Text.Text = "hello"
	text.Replay.Raw = json.RawMessage(`{"text":null,"thoughtSignature":"signed"}`)
	if _, _, err := Adapt(
		llm.Input{
			Messages: []llm.Message{llm.AssistantMessage(text)},
		},
		origin,
	); err == nil {
		t.Fatal("null Gemini text replay was accepted")
	}
}

func geminiToolReplay(id, raw string, origin content.Origin) llm.Input {
	messages := toolExchange(id)
	messages[0].Content[0].Replay = &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    "function_call",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(raw),
	}
	return llm.Input{Messages: messages}
}

func TestAdaptGeminiReplayRejectsForeignDestination(t *testing.T) {
	source := content.Origin{
		Provider: "google",
		API:      "google.generate_content",
		Model:    "source",
	}
	target := source
	target.Model = "target"
	input := geminiToolReplay(
		"call-1",
		`{"functionCall":{"id":"call-1","name":"lookup","args":{}}}`,
		source,
	)
	if _, _, err := Adapt(input, target); err == nil {
		t.Fatal("foreign Gemini replay was accepted for stripping")
	}
}
