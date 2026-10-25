package providercontent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestAdaptOmitsOnlyCompletedForeignProviderToolPair(t *testing.T) {
	foreign := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "source",
		Scope:    "source-scope",
	}
	target := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "target",
		Scope:    "target-scope",
	}
	use := llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "provider_tool_1",
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "server_tool_use"),
	}
	result := llm.Content{
		Kind: llm.ContentProviderToolResult,
		ProviderToolResult: &llm.ProviderToolResult{
			CallID: "provider_tool_1",
			Type:   "web_search_tool_result",
			Origin: foreign,
			Output: []content.Block{
				{
					Kind: content.KindResource,
					Resource: &content.Resource{
						URI:    "https://example.com",
						Origin: foreign,
					},
				},
			},
		},
		Replay: providerToolReplay(foreign, "web_search_tool_result"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignProviderToolActivity: llm.ProviderToolOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Search")),
			llm.AssistantMessage(use, result, llm.Text("Answer")),
			llm.UserMessage(llm.Text("Thanks")),
		},
	}
	adapted, changes, err := Adapt(input, target)
	if err != nil {
		t.Fatalf("omit completed provider tool pair: %v", err)
	}
	if len(changes) != 2 ||
		changes[0].Action != llm.ContentChangeOmitProviderToolActivity ||
		changes[1].Action != llm.ContentChangeOmitProviderToolActivity {
		t.Fatal("provider tool omission decisions were not recorded")
	}
	if len(adapted.Messages[1].Content) != 1 ||
		adapted.Messages[1].Text() != "Answer" {
		t.Fatal("provider tool omission changed portable answer text")
	}
}

func TestAdaptRejectsPendingForeignProviderToolUseDespiteOmitPolicy(t *testing.T) {
	foreign := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "source",
		Scope:    "source-scope",
	}
	target := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "target",
		Scope:    "target-scope",
	}
	use := llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "provider_tool_1",
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "server_tool_use"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignProviderToolActivity: llm.ProviderToolOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Search")),
			llm.AssistantMessage(use),
		},
	}
	if _, _, err := Adapt(input, target); err == nil {
		t.Fatal("pending foreign provider tool use was silently omitted")
	}
}

// A hosted MCP approval is answered in a user message, and the approved call
// follows in a later assistant message. Switching destination drops the
// whole exchange, including the messages it empties.
func TestAdaptOmitsAnsweredForeignProviderToolApproval(t *testing.T) {
	foreign := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "source",
		Scope:    "source-scope",
	}
	target := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "target",
		Scope:    "target-scope",
	}
	request := llm.Content{
		Kind: llm.ContentProviderToolApprovalRequest,
		ProviderToolApprovalRequest: &llm.ProviderToolApprovalRequest{
			ID:     "approval_1",
			Name:   "read",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "mcp_approval_request"),
	}
	response := llm.Content{
		Kind: llm.ContentProviderToolApprovalResponse,
		ProviderToolApprovalResponse: &llm.ProviderToolApprovalResponse{
			RequestID: "approval_1",
			Approve:   true,
			Origin:    foreign,
		},
	}
	call := llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "mcp_1",
			Name:   "read",
			Type:   "mcp_call",
			Status: "completed",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "mcp_call"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignProviderToolActivity: llm.ProviderToolOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Read the docs")),
			llm.AssistantMessage(request),
			llm.UserMessage(response),
			llm.AssistantMessage(call, llm.Text("Done")),
			llm.UserMessage(llm.Text("Thanks")),
		},
	}
	adapted, changes, err := Adapt(input, target)
	if err != nil {
		t.Fatalf("omit answered provider tool approval: %v", err)
	}
	want := []providerToolPosition{{1, 0}, {2, 0}, {3, 0}}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v", changes)
	}
	for i, change := range changes {
		if change.Action != llm.ContentChangeOmitProviderToolActivity ||
			(providerToolPosition{change.MessageIndex, change.ContentIndex}) != want[i] {
			t.Fatalf("change %d = %+v", i, change)
		}
	}
	if len(adapted.Messages) != 3 ||
		adapted.Messages[1].Role != llm.RoleAssistant ||
		adapted.Messages[1].Text() != "Done" {
		t.Fatalf("emptied messages were not dropped: %+v", adapted.Messages)
	}
}

func TestAdaptRejectsUnansweredForeignProviderToolApproval(t *testing.T) {
	foreign := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "source",
		Scope:    "source-scope",
	}
	target := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "target",
		Scope:    "target-scope",
	}
	request := llm.Content{
		Kind: llm.ContentProviderToolApprovalRequest,
		ProviderToolApprovalRequest: &llm.ProviderToolApprovalRequest{
			ID:     "approval_1",
			Name:   "read",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "mcp_approval_request"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignProviderToolActivity: llm.ProviderToolOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Read the docs")),
			llm.AssistantMessage(request),
			llm.UserMessage(llm.Text("Go on")),
		},
	}
	if _, _, err := Adapt(input, target); err == nil {
		t.Fatal("unanswered foreign approval request was silently omitted")
	}
}

func TestAdaptRejectsReusedProviderToolApprovalID(t *testing.T) {
	origin := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "test-model",
		Scope:    "test-scope",
	}
	request := llm.Content{
		Kind: llm.ContentProviderToolApprovalRequest,
		ProviderToolApprovalRequest: &llm.ProviderToolApprovalRequest{
			ID:     "approval_1",
			Name:   "read",
			Origin: origin,
		},
		Replay: providerToolReplay(origin, "mcp_approval_request"),
	}
	response := llm.Content{
		Kind: llm.ContentProviderToolApprovalResponse,
		ProviderToolApprovalResponse: &llm.ProviderToolApprovalResponse{
			RequestID: "approval_1",
			Origin:    origin,
		},
	}
	input := llm.Input{
		Model: "test-model",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Read the docs")),
			llm.AssistantMessage(request),
			llm.UserMessage(response),
			llm.AssistantMessage(request),
		},
	}
	if _, _, err := Adapt(input, origin); err == nil {
		t.Fatal("provider tool approval ID was reused after a decision")
	}
}

// Dropping a message emptied by omission must not leave two user or two
// assistant messages in a row. Messages that were already adjacent stay as
// they were, and tool results never join other content.
func TestAdaptJoinsMessagesLeftAdjacentByOmission(t *testing.T) {
	foreign := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "source",
		Scope:    "source-scope",
	}
	target := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "target",
		Scope:    "target-scope",
	}
	search := llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "ws_1",
			Name:   "web_search",
			Type:   "web_search_call",
			Status: "completed",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "web_search_call"),
	}
	request := llm.Content{
		Kind: llm.ContentProviderToolApprovalRequest,
		ProviderToolApprovalRequest: &llm.ProviderToolApprovalRequest{
			ID:     "approval_1",
			Name:   "read",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "mcp_approval_request"),
	}
	response := llm.Content{
		Kind: llm.ContentProviderToolApprovalResponse,
		ProviderToolApprovalResponse: &llm.ProviderToolApprovalResponse{
			RequestID: "approval_1",
			Approve:   true,
			Origin:    foreign,
		},
	}
	call := llm.Content{
		Kind: llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{
			ID:     "mcp_1",
			Name:   "read",
			Type:   "mcp_call",
			Status: "completed",
			Origin: foreign,
		},
		Replay: providerToolReplay(foreign, "mcp_call"),
	}
	// Reasoning without replay data cannot be sent to Messages.
	reasoning := llm.Content{
		Kind:      llm.ContentReasoning,
		Reasoning: &llm.Reasoning{Text: "plan"},
	}
	localCall := llm.Content{
		Kind: llm.ContentToolCall,
		ToolCall: &tool.Call{
			ID:   "call_1",
			Name: "lookup",
			Args: json.RawMessage(`{}`),
		},
	}
	localResult := llm.Content{
		Kind: llm.ContentToolResult,
		ToolResult: &llm.ToolResult{
			CallID: "call_1",
			Output: tool.TextOutput("found"),
		},
	}
	type message struct {
		role  llm.Role
		texts []string
	}
	for _, tc := range []struct {
		name     string
		messages []llm.Message
		want     []message
	}{
		{
			name: "lone web search",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("Search")),
				llm.AssistantMessage(search),
				llm.UserMessage(llm.Text("Thanks")),
			},
			want: []message{
				{llm.RoleUser, []string{"Search", "Thanks"}},
			},
		},
		{
			name: "approved call without text",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("Read the docs")),
				llm.AssistantMessage(request),
				llm.UserMessage(response),
				llm.AssistantMessage(call),
				llm.UserMessage(llm.Text("Thanks")),
			},
			want: []message{
				{llm.RoleUser, []string{"Read the docs", "Thanks"}},
			},
		},
		{
			name: "approval between assistant text",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("Read the docs")),
				llm.AssistantMessage(llm.Text("Asking"), request),
				llm.UserMessage(response),
				llm.AssistantMessage(call, llm.Text("Done")),
				llm.UserMessage(llm.Text("Thanks")),
			},
			want: []message{
				{llm.RoleUser, []string{"Read the docs"}},
				{llm.RoleAssistant, []string{"Asking", "Done"}},
				{llm.RoleUser, []string{"Thanks"}},
			},
		},
		{
			name: "reasoning only",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("Think")),
				llm.AssistantMessage(reasoning),
				llm.UserMessage(llm.Text("Go on")),
			},
			want: []message{
				{llm.RoleUser, []string{"Think", "Go on"}},
			},
		},
		{
			name: "already adjacent without omission",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("One")),
				llm.UserMessage(llm.Text("Two")),
				llm.AssistantMessage(llm.Text("Answer")),
			},
			want: []message{
				{llm.RoleUser, []string{"One"}},
				{llm.RoleUser, []string{"Two"}},
				{llm.RoleAssistant, []string{"Answer"}},
			},
		},
		{
			name: "already adjacent after omission",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("One")),
				llm.AssistantMessage(search),
				llm.UserMessage(llm.Text("Two")),
				llm.UserMessage(llm.Text("Three")),
			},
			want: []message{
				{llm.RoleUser, []string{"One", "Two"}},
				{llm.RoleUser, []string{"Three"}},
			},
		},
		{
			name: "tool result stays separate",
			messages: []llm.Message{
				llm.UserMessage(llm.Text("Look it up")),
				llm.AssistantMessage(localCall),
				llm.UserMessage(localResult),
				llm.AssistantMessage(search),
				llm.UserMessage(llm.Text("Thanks")),
			},
			want: []message{
				{llm.RoleUser, []string{"Look it up"}},
				{llm.RoleAssistant, []string{""}},
				{llm.RoleUser, []string{""}},
				{llm.RoleUser, []string{"Thanks"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := llm.Input{
				Model: "target",
				ContentPolicy: llm.ContentPolicy{
					UnreplayableReasoning:       llm.ReasoningOmit,
					ForeignProviderToolActivity: llm.ProviderToolOmit,
				},
				Messages: tc.messages,
			}
			adapted, changes, err := Adapt(input, target)
			if err != nil {
				t.Fatalf("adapt: %v", err)
			}
			var got []message
			for _, m := range adapted.Messages {
				var texts []string
				for _, c := range m.Content {
					text := ""
					if c.Block != nil && c.Block.Text != nil {
						text = c.Block.Text.Text
					}
					texts = append(texts, text)
				}
				got = append(got, message{m.Role, texts})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("adapted messages = %+v, want %+v", got, tc.want)
			}
			// Decisions keep their original input positions.
			for _, change := range changes {
				if change.MessageIndex >= len(input.Messages) ||
					change.ContentIndex >=
						len(input.Messages[change.MessageIndex].Content) {
					t.Fatalf("change %+v is outside the original input", change)
				}
			}
		})
	}
}

func providerToolReplay(origin content.Origin, typ string) *llm.Opaque {
	return &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    typ,
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"type":"` + typ + `"}`),
	}
}
