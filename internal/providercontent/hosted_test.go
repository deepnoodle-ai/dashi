package providercontent

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestAdaptOmitsOnlyCompletedForeignHostedPair(t *testing.T) {
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
		Kind: llm.ContentHostedUse,
		HostedUse: &llm.HostedUse{
			ID:     "hosted_1",
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: foreign,
		},
		Replay: hostedReplay(foreign, "server_tool_use"),
	}
	result := llm.Content{
		Kind: llm.ContentHostedResult,
		HostedResult: &llm.HostedResult{
			CallID: "hosted_1",
			Type:   "web_search_tool_result",
			Origin: foreign,
			Output: []content.Block{{
				Kind: content.KindResource,
				Resource: &content.Resource{
					URI:    "https://example.com",
					Origin: foreign,
				},
			}},
		},
		Replay: hostedReplay(foreign, "web_search_tool_result"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignHostedActivity: llm.HostedOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Search")),
			llm.AssistantMessage(use, result, llm.Text("Answer")),
			llm.UserMessage(llm.Text("Thanks")),
		},
	}
	adapted, changes, err := Adapt(input, target)
	if err != nil {
		t.Fatalf("omit completed hosted pair: %v", err)
	}
	if len(changes) != 2 ||
		changes[0].Action != llm.ContentChangeOmitHostedActivity ||
		changes[1].Action != llm.ContentChangeOmitHostedActivity {
		t.Fatal("hosted omission decisions were not recorded")
	}
	if len(adapted.Messages[1].Content) != 1 ||
		adapted.Messages[1].Text() != "Answer" {
		t.Fatal("hosted omission changed portable answer text")
	}
}

func TestAdaptRejectsPendingForeignHostedUseDespiteOmitPolicy(t *testing.T) {
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
		Kind: llm.ContentHostedUse,
		HostedUse: &llm.HostedUse{
			ID:     "hosted_1",
			Name:   "web_search",
			Type:   "server_tool_use",
			Origin: foreign,
		},
		Replay: hostedReplay(foreign, "server_tool_use"),
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			ForeignHostedActivity: llm.HostedOmit,
		},
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Search")),
			llm.AssistantMessage(use),
		},
	}
	if _, _, err := Adapt(input, target); err == nil {
		t.Fatal("pending foreign hosted use was silently omitted")
	}
}

func TestAdaptRejectsReusedHostedApprovalID(t *testing.T) {
	origin := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "test-model",
		Scope:    "test-scope",
	}
	request := llm.Content{
		Kind: llm.ContentHostedApprovalRequest,
		HostedApprovalRequest: &llm.HostedApprovalRequest{
			ID:     "approval_1",
			Name:   "read",
			Origin: origin,
		},
		Replay: hostedReplay(origin, "mcp_approval_request"),
	}
	response := llm.Content{
		Kind: llm.ContentHostedApprovalResponse,
		HostedApprovalResponse: &llm.HostedApprovalResponse{
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
		t.Fatal("hosted approval ID was reused after a decision")
	}
}

func hostedReplay(origin content.Origin, typ string) *llm.Opaque {
	return &llm.Opaque{
		Version: 1,
		Origin:  origin,
		Type:    typ,
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     json.RawMessage(`{"type":"` + typ + `"}`),
	}
}
