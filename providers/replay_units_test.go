package providers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPrepareChecksReplayUnitAndCompleteness(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://example.com/api")
			origin := preparedRequest(t, client).Origin()
			for _, unit := range []llm.OpaqueUnit{llm.OpaqueUnitItem, llm.OpaqueUnitContentBlock, llm.OpaqueUnitMessagePart} {
				t.Run(string(unit), func(t *testing.T) {
					raw := json.RawMessage(`{"type":"future_item","private":9007199254740993}`)
					if unit == llm.OpaqueUnitMessagePart {
						raw = json.RawMessage(`{"message":{"type":"message","id":"m","role":"assistant"},"part":{"type":"future_part","private":9007199254740993}}`)
					}
					o := &llm.Opaque{Version: 1, Origin: origin, Type: "future_type", Unit: unit, Raw: raw}
					input := modelInput()
					input.Messages = append(input.Messages, llm.Message{Role: llm.RoleAssistant,
						Content: []llm.Content{{Kind: llm.ContentOpaque, Opaque: o}}})
					encoded, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					var restored llm.Input
					if err := json.Unmarshal(encoded, &restored); err != nil {
						t.Fatal(err)
					}
					req, err := client.Prepare(context.Background(), restored)
					supported := (tc.name == "anthropic" && unit == llm.OpaqueUnitContentBlock) ||
						(tc.name == "openai" && unit != llm.OpaqueUnitContentBlock)
					if (err == nil) != supported {
						t.Fatalf("unit support = %t, want %t: %v", err == nil, supported, err)
					}
					if supported && (!strings.Contains(string(req.Body()), "9007199254740993") || !strings.Contains(string(req.Body()), "future_")) {
						t.Fatal("opaque replay changed unfamiliar provider data")
					}
					o.Partial = true
					if _, err := client.Prepare(context.Background(), input); err == nil {
						t.Fatal("recognized unit permitted partial replay")
					}
				})
			}
		})
	}
}

func TestPrepareRejectsPartialAttachmentsOnKnownContent(t *testing.T) {
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.new("https://example.com/api")
			origin := preparedRequest(t, client).Origin()
			for _, kind := range []string{"text", "reasoning"} {
				t.Run(kind, func(t *testing.T) {
					o := &llm.Opaque{Version: 1, Origin: origin, Unit: llm.OpaqueUnitContentBlock, Type: "text",
						Raw: json.RawMessage(`{"type":"text","text":"hello"}`)}
					if kind == "reasoning" {
						o.Type = "thinking"
						o.Raw = json.RawMessage(`{"type":"thinking","thinking":"thought","signature":"signed"}`)
					}
					if tc.name == "openai" {
						o.Unit = llm.OpaqueUnitMessagePart
						o.Type = "output_text"
						o.Raw = json.RawMessage(`{"message":{"type":"message","id":"m"},"part":{"type":"output_text","text":"hello"}}`)
						if kind == "reasoning" {
							o.Unit, o.Type = llm.OpaqueUnitItem, "reasoning"
							o.Raw = json.RawMessage(`{"type":"reasoning","encrypted_content":"private","summary":[]}`)
						}
					}
					c := llm.Text("hello")
					c.Replay = o
					if kind == "reasoning" {
						c = llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{}, Replay: o}
					}
					input := modelInput()
					input.Messages = append(input.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{c}})
					if _, err := client.Prepare(context.Background(), input); err != nil {
						t.Fatalf("complete attachment rejected: %v", err)
					}
					o.Partial = true
					if _, err := client.Prepare(context.Background(), input); err == nil {
						t.Fatal("known content bypassed partial replay rejection")
					}
				})
			}
		})
	}
}
