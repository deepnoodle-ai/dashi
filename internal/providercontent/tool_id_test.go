package providercontent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestAdaptRemapsPairedToolIDsWithoutChangingInput(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
	}
	input := llm.Input{Messages: toolExchange("call.with.dot")}
	original, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out, changes, err := Adapt(input, origin)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("call.with.dot"))
	want := "call_" + hex.EncodeToString(hash[:16])
	if got := out.Messages[0].Content[0].ToolCall.ID; got != want {
		t.Fatalf("tool call ID = %q, want %q", got, want)
	}
	if got := out.Messages[1].Content[0].ToolResult.CallID; got != want {
		t.Fatalf("tool result ID = %q, want %q", got, want)
	}
	after, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("adaptation changed saved input")
	}
	if len(changes) != 2 {
		t.Fatalf(
			"changes = %+v, want one for each original position",
			changes,
		)
	}
	for i, change := range changes {
		if change.MessageIndex != i || change.ContentIndex != 0 ||
			change.Action != llm.ContentChangeRemapToolID ||
			change.FromID != "call.with.dot" ||
			change.ToID != want {
			t.Fatalf("change %d = %+v", i, change)
		}
	}
}

func TestAdaptReservesLaterSupportedToolIDs(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
	}
	unsupported := "call.with.dot"
	hash := sha256.Sum256([]byte(unsupported))
	firstCandidate := "call_" + hex.EncodeToString(hash[:16])
	messages := append(toolExchange(unsupported), toolExchange(firstCandidate)...)
	out, _, err := Adapt(llm.Input{Messages: messages}, origin)
	if err != nil {
		t.Fatal(err)
	}
	got := out.Messages[0].Content[0].ToolCall.ID
	if got == firstCandidate || !portableCallID(got) {
		t.Fatalf("remapped ID %q displaced a later original ID", got)
	}
	if out.Messages[1].Content[0].ToolResult.CallID != got {
		t.Fatal("remapped call and result were not paired")
	}
	if out.Messages[2].Content[0].ToolCall.ID != firstCandidate ||
		out.Messages[3].Content[0].ToolResult.CallID != firstCandidate {
		t.Fatal("later supported ID was changed")
	}
	other := append(toolExchange(firstCandidate), toolExchange(unsupported)...)
	reordered, _, err := Adapt(llm.Input{Messages: other}, origin)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Messages[2].Content[0].ToolCall.ID != got {
		t.Fatal("allocation depended on exchange order")
	}
}

func TestAdaptResolvesGeneratedToolIDCollisions(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
	}
	first := "call.with.dot"
	second := first + "\x00" + "1"
	hash := sha256.Sum256([]byte(first))
	reserved := "call_" + hex.EncodeToString(hash[:16])
	messages := append(toolExchange(first), toolExchange(second)...)
	messages = append(messages, toolExchange(reserved)...)
	out, _, err := Adapt(llm.Input{Messages: messages}, origin)
	if err != nil {
		t.Fatal(err)
	}
	firstID := out.Messages[0].Content[0].ToolCall.ID
	secondID := out.Messages[2].Content[0].ToolCall.ID
	if firstID == secondID || firstID == reserved || secondID == reserved {
		t.Fatalf(
			"generated IDs collided: %q, %q, reserved %q",
			firstID,
			secondID,
			reserved,
		)
	}
	if out.Messages[1].Content[0].ToolResult.CallID != firstID ||
		out.Messages[3].Content[0].ToolResult.CallID != secondID {
		t.Fatal("generated IDs lost their paired results")
	}
	reversed := append(toolExchange(reserved), toolExchange(second)...)
	reversed = append(reversed, toolExchange(first)...)
	reordered, _, err := Adapt(llm.Input{Messages: reversed}, origin)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Messages[2].Content[0].ToolCall.ID != secondID ||
		reordered.Messages[4].Content[0].ToolCall.ID != firstID {
		t.Fatal("generated collision resolution depended on exchange order")
	}
}

func TestAdaptRemapChecksDependentReplayFields(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
	}
	messages := toolExchange("call.with.dot")
	call := &messages[0].Content[0]
	call.Replay = &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    "tool_use",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"type":"tool_use","id":"call.with.dot","name":"lookup","input":{},"related_call_id":"call.with.dot"}`),
	}
	if _, _, err := Adapt(
		llm.Input{
			Messages: messages,
		},
		origin,
	); err == nil {
		t.Fatal("remap accepted an unfamiliar dependent replay field")
	}
	call.Replay.Raw = json.RawMessage(`{"type":"tool_use","id":"call.with.dot","name":"lookup","input":{}}`)
	out, _, err := Adapt(llm.Input{Messages: messages}, origin)
	if err != nil {
		t.Fatalf("known replay was rejected: %v", err)
	}
	if string(out.Messages[0].Content[0].Replay.Raw) != string(call.Replay.Raw) {
		t.Fatal("remap rewrote raw replay bytes")
	}
	if out.Messages[0].Content[0].ToolCall.ID == "call.with.dot" {
		t.Fatal("canonical ID was not remapped")
	}
	foreign := origin
	foreign.Model = "other"
	call.Replay.Origin = foreign
	out, changes, err := Adapt(llm.Input{Messages: messages}, origin)
	if err != nil {
		t.Fatalf("portable foreign replay was rejected: %v", err)
	}
	if out.Messages[0].Content[0].Replay != nil {
		t.Fatal("foreign replay was retained")
	}
	if len(changes) != 3 ||
		changes[0].Action != llm.ContentChangeRemapToolID ||
		changes[1].Action != llm.ContentChangeStripReplay ||
		changes[2].Action != llm.ContentChangeRemapToolID {
		t.Fatalf("remap and strip decisions = %+v", changes)
	}
}

func TestAdaptRemapKeepsIndependentResponsesItemID(t *testing.T) {
	origin := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "m",
	}
	messages := toolExchange("call.with.dot")
	raw := json.RawMessage(`{"type":"function_call","id":"fc_private","call_id":"call.with.dot","name":"lookup","arguments":"{}","status":"completed"}`)
	messages[0].Content[0].Replay = &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    "function_call",
		Unit:    llm.OpaqueUnitItem,
		Raw:     raw,
	}
	out, _, err := Adapt(llm.Input{Messages: messages}, origin)
	if err != nil {
		t.Fatal(err)
	}
	call := out.Messages[0].Content[0]
	if call.ToolCall.ID == "call.with.dot" {
		t.Fatal("canonical call ID was not remapped")
	}
	if string(call.Replay.Raw) != string(raw) {
		t.Fatal("private item ID or raw replay was rewritten")
	}
}

func TestAdaptRemapPreservesPairingValidation(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
	}
	for _, tc := range []struct {
		name     string
		messages []llm.Message
	}{
		{name: "unanswered", messages: toolExchange("bad.id")[:1]},
		{name: "orphan", messages: toolExchange("bad.id")[1:]},
		{
			name:     "duplicate call",
			messages: append(toolExchange("bad.id"), toolExchange("bad.id")...),
		},
		{name: "empty call", messages: toolExchange("")},
		{
			name: "empty result",
			messages: func() []llm.Message {
				messages := toolExchange("bad.id")
				messages[1].Content[0].ToolResult.CallID = ""
				return messages
			}(),
		},
		{name: "invalid UTF-8", messages: toolExchange("bad\xffid")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Adapt(
				llm.Input{
					Messages: tc.messages,
				},
				origin,
			); err == nil {
				t.Fatal("invalid tool exchange was accepted")
			}
		})
	}
}

func toolExchange(id string) []llm.Message {
	return []llm.Message{
		llm.AssistantMessage(llm.Content{
			Kind: llm.ContentToolCall,
			ToolCall: &tool.Call{
				ID:   id,
				Name: "lookup",
				Args: json.RawMessage(`{}`),
			},
		}),
		llm.UserMessage(llm.Content{
			Kind:       llm.ContentToolResult,
			ToolResult: &llm.ToolResult{CallID: id},
		}),
	}
}
