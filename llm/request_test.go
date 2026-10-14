package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
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

func TestNewRequestRejectsUnresolvedContentReference(t *testing.T) {
	ref := content.Ref{
		ID:     "upl_1",
		SHA256: strings.Repeat("a", 64),
		Size:   3,
	}
	input := Input{
		Model: "m",
		Messages: []Message{UserMessage(Image(content.Source{
			Ref:      &ref,
			MIMEType: "image/png",
		}))},
	}
	if !input.HasRef() {
		t.Fatal("HasRef missed the reference")
	}

	_, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "p",
			API:      "a",
			Model:    "m",
			Scope:    "s",
		},
		Input:     input,
		Effective: input.Settings,
		Body:      []byte(`{}`),
	})

	if err == nil {
		t.Fatal("a request with an unresolved reference was accepted")
	}
}

func TestNewRequestRejectsUnresolvedReferenceInProviderToolUseOutput(t *testing.T) {
	ref := content.Ref{ID: "upl_1", SHA256: strings.Repeat("a", 64), Size: 3}
	origin := content.Origin{Provider: "p", API: "a", Model: "m", Scope: "s"}
	input := Input{
		Model: "m",
		Messages: []Message{UserMessage(Content{
			Kind: ContentProviderToolUse,
			ProviderToolUse: &ProviderToolUse{
				ID:     "h1",
				Name:   "fetch",
				Type:   "tool_use",
				Origin: origin,
				Output: []content.Block{content.ImageBlock(content.Source{
					Ref:      &ref,
					MIMEType: "image/png",
				})},
			},
		})},
	}
	if !input.HasRef() {
		t.Fatal("HasRef missed the provider tool use reference")
	}

	_, err := NewRequest(RequestSnapshot{
		Origin:    origin,
		Input:     input,
		Effective: input.Settings,
		Body:      []byte(`{}`),
	})

	if err == nil {
		t.Fatal("a request with an unresolved provider tool use reference was accepted")
	}
}

// TestRequestAccessorsMatchRecordedSnapshot checks that the decoded snapshot
// each accessor reads is what a recorded snapshot restores to.
func TestRequestAccessorsMatchRecordedSnapshot(t *testing.T) {
	req := testRequest(t, 6)
	var want RequestSnapshot
	if err := json.Unmarshal(req.snapshot, &want); err != nil {
		t.Fatal(err)
	}
	want.Body = req.Body()
	if got := req.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %#v; want %#v", got, want)
	}
	if !reflect.DeepEqual(req.Input(), want.Input) ||
		req.Origin() != want.Origin ||
		!reflect.DeepEqual(req.Effective(), want.Effective) ||
		!reflect.DeepEqual(req.Warnings(), want.Warnings) {
		t.Fatal("an accessor differs from the recorded snapshot")
	}
	if got := (Request{}).Snapshot(); !reflect.DeepEqual(got, RequestSnapshot{}) {
		t.Fatalf("zero Request snapshot = %#v", got)
	}
}

func TestRequestAccessorsReturnOwnedValues(t *testing.T) {
	req := testRequest(t, 2)
	in := req.Input()
	in.Messages[0].Content[0].Block.Text.Text = "changed"
	in.Tools[0].Schema[0] = '['
	snapshot := req.Snapshot()
	snapshot.Input.Messages = nil
	effective := req.Effective()
	*effective.Temperature = 2
	if got := req.Input(); got.Messages[0].Text() != "start" ||
		got.Tools[0].Schema[0] != '{' ||
		*req.Effective().Temperature != 0.5 ||
		len(req.Snapshot().Input.Messages) == 0 {
		t.Fatal("changing a returned value changed the request")
	}
}

func TestRequestAccessorsAreSafeConcurrently(t *testing.T) {
	req := testRequest(t, 4)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			in := req.Input()
			in.Messages[0].Content[0].Block.Text.Text = "changed"
			_ = req.Snapshot()
			_ = req.Origin()
		})
	}
	wg.Wait()
	if req.Input().Messages[0].Text() != "start" {
		t.Fatal("a concurrent reader changed the request")
	}
}

// testRequest prepares a request whose history has about the given number
// of messages, alternating tool calls and tool results.
func testRequest(tb testing.TB, messages int) Request {
	tb.Helper()
	temperature := 0.5
	settings := Settings{MaxTokens: 1000, Temperature: &temperature}
	input := Input{
		Model:  "m",
		System: strings.Repeat("You are a careful assistant. ", 40),
		Tools: []tool.Definition{{
			Name:   "act",
			Schema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}}}`),
		}},
		Messages: []Message{UserMessage(Text("start"))},
		Settings: settings,
	}
	for i := range messages / 2 {
		id := fmt.Sprintf("call-%d", i)
		input.Messages = append(input.Messages,
			AssistantMessage(Text(strings.Repeat("thinking out loud ", 20)), Content{
				Kind:     ContentToolCall,
				ToolCall: &tool.Call{ID: id, Name: "act", Args: json.RawMessage(`{"limit":1}`)},
			}),
			UserMessage(Content{
				Kind: ContentToolResult,
				ToolResult: &ToolResult{
					CallID: id,
					Output: tool.TextOutput(strings.Repeat("result line\n", 40)),
				},
			}),
		)
	}
	req, err := NewRequest(RequestSnapshot{
		Origin:    content.Origin{Provider: "p", API: "api", Model: "m", Scope: "s"},
		Input:     input,
		Effective: settings,
		Body:      []byte(`{}`),
	})
	if err != nil {
		tb.Fatal(err)
	}
	return req
}

// BenchmarkRequestAccessors reads a request the way one model call does:
// the runner, budget checks, and provider each ask for parts of it.
func BenchmarkRequestAccessors(b *testing.B) {
	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("messages=%d", n), func(b *testing.B) {
			req := testRequest(b, n)
			b.ReportAllocs()
			for b.Loop() {
				_ = req.Origin()
				_ = req.Snapshot()
				_ = req.Effective()
				_ = req.Input()
				_ = req.Snapshot()
				_ = req.Warnings()
			}
		})
	}
}
