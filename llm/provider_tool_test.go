package llm

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func TestProviderToolApprovalResponseRejectsProviderReplay(t *testing.T) {
	origin := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "model",
		Scope:    "scope",
	}
	response := Content{
		Kind: ContentProviderToolApprovalResponse,
		ProviderToolApprovalResponse: &ProviderToolApprovalResponse{
			RequestID: "approval_1",
			Origin:    origin,
		},
		Replay: &Opaque{
			Version: 1,
			Origin:  origin,
			Type:    "mcp_approval_response",
			Unit:    OpaqueUnitItem,
			Raw:     json.RawMessage(`{"type":"mcp_approval_response"}`),
		},
	}
	if err := response.Validate(); err == nil {
		t.Fatal("caller-authored approval accepted provider replay data")
	}
}
