package llm

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/tool"
)

func TestContentValidatePreservesToolResultIDThroughJSON(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		valid bool
	}{
		{name: "ASCII", id: "call-1", valid: true},
		{name: "Unicode", id: "工具:1", valid: true},
		{name: "invalid UTF-8", id: "call-\xff"},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part := Content{
				Kind: ContentToolResult,
				ToolResult: &ToolResult{
					CallID: tt.id,
					Output: tool.TextOutput("done"),
				},
			}
			if err := part.Validate(); (err == nil) != tt.valid {
				t.Fatalf("result ID validation disagrees with JSON safety: %v", err)
			}
			if !tt.valid {
				return
			}
			data, err := json.Marshal(part)
			if err != nil {
				t.Fatal(err)
			}
			var restored Content
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.ToolResult.CallID != tt.id {
				t.Fatal("JSON changed the original result association")
			}
		})
	}
}
