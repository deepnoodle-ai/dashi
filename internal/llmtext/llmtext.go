// Package llmtext reads the text out of llm content.
package llmtext

import (
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Join concatenates text blocks. Adjacent blocks are joined as they are,
// because providers split one cited sentence into several blocks. Any other
// item between two text blocks (a provider tool use, a tool call, reasoning)
// ends a passage, so the next text starts after a blank line.
func Join(parts []llm.Content) string {
	var out strings.Builder
	gap := false
	for _, part := range parts {
		if part.Kind != llm.ContentBlock ||
			part.Block == nil ||
			part.Block.Text == nil {
			gap = true
			continue
		}
		if gap && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n\n")
		}
		gap = false
		out.WriteString(part.Block.Text.Text)
	}
	return out.String()
}
