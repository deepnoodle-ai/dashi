package llm

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestRequestSnapshotUsesAcceptedFieldNames(t *testing.T) {
	input := Input{
		Model: "m",
		Messages: []Message{
			UserMessage(Text("hello")),
			AssistantMessage(Content{
				Kind: ContentToolCall,
				ToolCall: &tool.Call{
					ID:            "call",
					Name:          "act",
					Args:          json.RawMessage(`{"limit":1}`),
					RequestedArgs: json.RawMessage(`{"limit":2}`),
				},
			}),
		},
	}
	body := []byte("{\n  \"replay\": 9007199254740993\n}")
	req, err := NewRequest(RequestSnapshot{
		Input: input,
		Body:  body,
		Origin: content.Origin{
			Provider: "p",
			API:      "api",
			Model:    "m",
			Scope:    "scope",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(req.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"input":`,
		`"kind":"block"`,
		`"block":`,
		`"requested_args":`,
	} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("snapshot omitted accepted field %s", field)
		}
	}
	for _, field := range []string{
		`"request":`,
		`"kind":"data"`,
		`"data":`,
		`"requested":`,
	} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatalf("snapshot retained legacy field %s", field)
		}
	}
	var snapshot RequestSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRequest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(
		restored.Body(),
		body,
	) || restored.Input().Messages[0].Text() != "hello" ||
		string(restored.Input().Messages[1].Content[0].ToolCall.RequestedArgs) != `{"limit":2}` {
		t.Fatal("record round trip lost exact body, shared content, or requested arguments")
	}
}

func TestNewRequestRejectsLegacySnapshotWithoutMigration(t *testing.T) {
	legacy := []byte(`{"origin":{"provider":"p","api":"api","model":"m","scope":"scope"},"request":{"model":"m"},"effective":{},"body":"e30="}`)
	var snapshot RequestSnapshot
	if err := json.Unmarshal(legacy, &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRequest(snapshot); err == nil {
		t.Fatal("legacy snapshot was silently interpreted as a prepared request")
	}
}

func TestNewRequestValidatesAndOwnsContentChanges(t *testing.T) {
	origin := content.Origin{
		Provider: "p",
		API:      "api",
		Model:    "target",
		Scope:    "scope",
	}
	foreign := origin
	foreign.Model = "source"
	part := Text("hello")
	part.Replay = &Opaque{
		Version: 1,
		Origin:  foreign,
		Type:    "text",
		Unit:    OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"type":"text","text":"hello"}`),
	}
	input := Input{
		Model: "target",
		Messages: []Message{
			AssistantMessage(part),
		},
	}
	change := ContentChange{
		MessageIndex: 0,
		ContentIndex: 0,
		Action:       ContentChangeStripReplay,
		Reason:       "foreign model",
	}
	for _, variant := range []string{
		"valid",
		"negative-message",
		"missing-content",
		"missing-reason",
		"unknown-action",
		"duplicate",
		"same-origin",
		"wrong-role",
	} {
		t.Run(variant, func(t *testing.T) {
			p := RequestSnapshot{
				Origin: origin,
				Input:  input,
				Body:   []byte(`{}`),
				ContentChanges: []ContentChange{
					change,
				},
			}
			switch variant {
			case "negative-message":
				p.ContentChanges[0].MessageIndex = -1
			case "missing-content":
				p.ContentChanges[0].ContentIndex = 1
			case "missing-reason":
				p.ContentChanges[0].Reason = ""
			case "unknown-action":
				p.ContentChanges[0].Action = "omit_everything"
			case "duplicate":
				p.ContentChanges = append(
					p.ContentChanges,
					change,
				)
			case "same-origin":
				p.Origin, p.Input.Model = foreign, foreign.Model
			case "wrong-role":
				p.ContentChanges[0].FromRole = RoleAssistant
			}
			request, err := NewRequest(p)
			if (err == nil) != (variant == "valid") {
				t.Fatalf(
					"content change acceptance for %s: %v",
					variant,
					err,
				)
			}
			if err != nil {
				return
			}
			p.ContentChanges[0].Reason = "modified"
			copy := request.ContentChanges()
			copy[0].Reason = "also modified"
			if request.ContentChanges()[0].Reason != "foreign model" {
				t.Fatal("content decisions share mutable storage")
			}
		})
	}
}
