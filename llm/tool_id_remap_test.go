package llm

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestRequestToolIDRemapRoundTripAndOwnership(t *testing.T) {
	snapshot := remapSnapshot()
	request, err := NewRequest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ContentChanges[0].FromID = "changed"
	snapshot.Input.Messages[0].Content[0].ToolCall.ID = "changed"
	snapshot.Body[0] = 'x'
	changes := request.ContentChanges()
	changes[0].ToID = "changed"
	got := request.Snapshot()
	if got.ContentChanges[0].FromID != "source:one" ||
		got.ContentChanges[0].ToID != "call_111" ||
		got.Input.Messages[0].Content[0].ToolCall.ID != "source:one" {
		t.Fatal("remap record or original input shared mutable storage")
	}
	if !bytes.Equal(got.Body, []byte("{\n  \"tool\": \"remapped\"\n}")) {
		t.Fatal("prepared body lost its exact bytes")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"action":"remap_tool_id"`,
		`"from_id":"source:one"`,
		`"to_id":"call_111"`,
	} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("record omitted %s", field)
		}
	}
	var decoded RequestSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRequest(decoded); err != nil {
		t.Fatalf("record could not be restored: %v", err)
	}
}

func TestNewRequestRejectsTamperedToolIDRemaps(t *testing.T) {
	tests := []struct {
		name   string
		change func(*RequestSnapshot)
	}{
		{
			name: "missing source",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].FromID = ""
			},
		},
		{
			name: "missing target",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].ToID = ""
			},
		},
		{
			name: "same IDs",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].ToID = "source:one"
			},
		},
		{
			name: "wrong call source",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].FromID = "source:two"
			},
		},
		{
			name: "wrong result source",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[1].FromID = "source:two"
			},
		},
		{
			name: "wrong call role",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[0].Role = RoleUser
			},
		},
		{
			name: "wrong result role",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[1].Role = RoleAssistant
			},
		},
		{
			name: "wrong kind",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[0].Content[0] = Text("ordinary text")
			},
		},
		{
			name: "duplicate decision",
			change: func(p *RequestSnapshot) {
				p.ContentChanges = append(
					p.ContentChanges,
					p.ContentChanges[0],
				)
			},
		},
		{
			name: "call without result decision",
			change: func(p *RequestSnapshot) {
				p.ContentChanges = append(p.ContentChanges[:1], p.ContentChanges[2:]...)
			},
		},
		{
			name: "result without call decision",
			change: func(p *RequestSnapshot) {
				p.ContentChanges = p.ContentChanges[1:]
			},
		},
		{
			name: "conflicting pair",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[1].ToID = "call_other"
			},
		},
		{
			name: "target matches retained call",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].ToID = "retained"
				p.ContentChanges[1].ToID = "retained"
			},
		},
		{
			name: "target matches another mapping",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[2].ToID = "call_111"
				p.ContentChanges[3].ToID = "call_111"
			},
		},
		{
			name: "result before call",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[0], p.Input.Messages[1] =
					p.Input.Messages[1], p.Input.Messages[0]
				p.ContentChanges[0].MessageIndex = 1
				p.ContentChanges[1].MessageIndex = 0
			},
		},
		{
			name: "duplicate source calls",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[4].Content[0].ToolCall.ID = "source:one"
				p.ContentChanges[2].FromID = "source:one"
			},
		},
		{
			name: "duplicate source results",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[5].Content[0].ToolResult.CallID = "source:one"
				p.ContentChanges[3].FromID = "source:one"
			},
		},
		{
			name: "nonportable target",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].ToID = "call:one"
			},
		},
		{
			name: "oversized target",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].ToID = strings.Repeat(
					"a",
					65,
				)
			},
		},
		{
			name: "invalid UTF-8 source",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].FromID = "\xff"
			},
		},
		{
			name: "invalid UTF-8 retained ID",
			change: func(p *RequestSnapshot) {
				p.Input.Messages[2].Content[0].ToolCall.ID = "\xff"
			},
		},
		{
			name: "role fields on remap",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].FromRole = RoleAssistant
			},
		},
		{
			name: "ID fields on other action",
			change: func(p *RequestSnapshot) {
				p.ContentChanges[0].Action = ContentChangeEncodeToolError
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := remapSnapshot()
			tc.change(&p)
			if _, err := NewRequest(p); err == nil {
				t.Fatal("tampered remap record was accepted")
			}
		})
	}
}

func TestNewRequestPreservesToolHistoryAcceptanceWithoutRemaps(t *testing.T) {
	p := remapSnapshot()
	p.ContentChanges = nil
	p.Input.Messages[1].Content[0].ToolResult.CallID = "orphan"
	if _, err := NewRequest(p); err != nil {
		t.Fatalf("unrelated tool history rule changed: %v", err)
	}
}

func TestNewRequestAllowsRecordedRemapOfPortableSourceID(t *testing.T) {
	p := remapSnapshot()
	p.Input.Messages[0].Content[0].ToolCall.ID = "source_one"
	p.Input.Messages[1].Content[0].ToolResult.CallID = "source_one"
	p.ContentChanges[0].FromID = "source_one"
	p.ContentChanges[1].FromID = "source_one"
	if _, err := NewRequest(p); err != nil {
		t.Fatalf(
			"recorded remap of a portable source ID was rejected: %v",
			err,
		)
	}
}

func TestNewRequestPreservesUnicodeToolIDInRecord(t *testing.T) {
	p := remapSnapshot()
	p.Input.Messages[0].Content[0].ToolCall.ID = "source:é"
	p.Input.Messages[1].Content[0].ToolResult.CallID = "source:é"
	p.ContentChanges[0].FromID = "source:é"
	p.ContentChanges[1].FromID = "source:é"
	request, err := NewRequest(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Snapshot().ContentChanges[0].FromID; got != "source:é" {
		t.Fatalf("original Unicode ID changed: %q", got)
	}
}

func TestNewRequestAllowsRemapWithOtherContentDecision(t *testing.T) {
	p := remapSnapshot()
	p.Input.Messages[3].Content[0].ToolResult.IsError = true
	p.ContentChanges = append(
		p.ContentChanges,
		ContentChange{
			MessageIndex: 3,
			ContentIndex: 0,
			Action:       ContentChangeEncodeToolError,
			Reason:       "tool error as text",
		},
	)
	if _, err := NewRequest(p); err != nil {
		t.Fatalf("independent content decisions conflict: %v", err)
	}
}

func remapSnapshot() RequestSnapshot {
	call := func(id string) Content {
		return Content{
			Kind: ContentToolCall,
			ToolCall: &tool.Call{
				ID:   id,
				Name: "act",
				Args: json.RawMessage(`{}`),
			},
		}
	}
	result := func(id string) Content {
		return Content{
			Kind:       ContentToolResult,
			ToolResult: &ToolResult{CallID: id},
		}
	}
	return RequestSnapshot{
		Origin: content.Origin{
			Provider: "p",
			API:      "api",
			Model:    "m",
			Scope:    "scope",
		},
		Input: Input{
			Model: "m",
			Messages: []Message{
				AssistantMessage(call("source:one")),
				UserMessage(result("source:one")),
				AssistantMessage(call("retained")),
				UserMessage(result("retained")),
				AssistantMessage(call("source:two")),
				UserMessage(result("source:two")),
			},
		},
		Body: []byte("{\n  \"tool\": \"remapped\"\n}"),
		ContentChanges: []ContentChange{
			{
				MessageIndex: 0,
				ContentIndex: 0,
				Action:       ContentChangeRemapToolID,
				Reason:       "portable tool ID",
				FromID:       "source:one",
				ToID:         "call_111",
			},
			{
				MessageIndex: 1,
				ContentIndex: 0,
				Action:       ContentChangeRemapToolID,
				Reason:       "portable tool ID",
				FromID:       "source:one",
				ToID:         "call_111",
			},
			{
				MessageIndex: 4,
				ContentIndex: 0,
				Action:       ContentChangeRemapToolID,
				Reason:       "portable tool ID",
				FromID:       "source:two",
				ToID:         "call_222",
			},
			{
				MessageIndex: 5,
				ContentIndex: 0,
				Action:       ContentChangeRemapToolID,
				Reason:       "portable tool ID",
				FromID:       "source:two",
				ToID:         "call_222",
			},
		},
	}
}
