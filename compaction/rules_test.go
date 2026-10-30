package compaction

import (
	"math"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A provider tool approval response answers the request before it, so the
// kept tail never starts at the response.
func TestTailStartKeepsProviderToolApprovalPairs(t *testing.T) {
	request := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{
		Kind:                        llm.ContentProviderToolApprovalRequest,
		ProviderToolApprovalRequest: &llm.ProviderToolApprovalRequest{ID: "r1", Name: "search"},
	}}}
	response := llm.Message{Role: llm.RoleUser, Content: []llm.Content{{
		Kind:                         llm.ContentProviderToolApprovalResponse,
		ProviderToolApprovalResponse: &llm.ProviderToolApprovalResponse{RequestID: "r1", Approve: true},
	}}}
	messages := []llm.Message{
		llm.UserMessage(llm.Text("first")),
		llm.AssistantMessage(llm.Text("answer")),
		llm.UserMessage(llm.Text("second")),
		request,
		response,
		llm.AssistantMessage(llm.Text("done")),
	}
	budget := estimate(response) + estimate(messages[5])
	if cut := tailStart(messages, budget); cut != len(messages) {
		t.Fatalf("cut at %d splits the approval pair", cut)
	}
	if !splitsExchange(messages[4:]) {
		t.Fatal("a response without its request does not split the exchange")
	}
	if splitsExchange(messages[3:]) {
		t.Fatal("a request and its response split the exchange")
	}
}

// A provider tool result answers the use before it, so a tail that starts
// between them splits the exchange.
func TestSplitsExchangeKeepsProviderToolPairs(t *testing.T) {
	use := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{
		Kind:            llm.ContentProviderToolUse,
		ProviderToolUse: &llm.ProviderToolUse{ID: "p1", Name: "web_search"},
	}}}
	result := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{{
		Kind:               llm.ContentProviderToolResult,
		ProviderToolResult: &llm.ProviderToolResult{CallID: "p1"},
	}}}
	messages := []llm.Message{use, llm.UserMessage(llm.Text("reminder")), result}
	if !splitsExchange(messages[1:]) {
		t.Fatal("a provider tool result without its use does not split the exchange")
	}
	if splitsExchange(messages) {
		t.Fatal("a provider tool use and its result split the exchange")
	}
}

// Sizes saturate, so two messages too large to estimate never add up to
// a tail that looks small enough to keep.
func TestTailStartSaturatesSizes(t *testing.T) {
	huge := llm.Message{Role: llm.RoleUser, Content: []llm.Content{
		llm.Image(content.Source{Ref: &content.Ref{ID: "huge", Size: math.MaxInt64}, MIMEType: "image/png"}),
	}}
	messages := []llm.Message{
		llm.UserMessage(llm.Text("first")),
		huge,
		huge,
		llm.UserMessage(llm.Text("last")),
	}
	if cut := tailStart(messages, 1000); cut != 3 {
		t.Fatalf("cut at %d; want 3", cut)
	}
}
