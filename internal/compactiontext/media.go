package compactiontext

import (
	"slices"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/mediakind"
	"github.com/deepnoodle-ai/dashi/llm"
)

// HasMedia reports content whose encoded bytes do not establish context
// tokens, including content nested in local and provider tool results.
func HasMedia(messages []llm.Message) bool {
	has := func(blocks []content.Block) bool {
		return slices.ContainsFunc(blocks, mediakind.IsMedia)
	}
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Block != nil && mediakind.IsMedia(*part.Block) ||
				part.ToolResult != nil && has(part.ToolResult.Output.Content) ||
				part.ProviderToolUse != nil && has(part.ProviderToolUse.Output) ||
				part.ProviderToolResult != nil && has(part.ProviderToolResult.Output) {
				return true
			}
		}
	}
	return false
}
