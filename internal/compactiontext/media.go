package compactiontext

import (
	"bytes"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// IsTextDocument qualifies inline text for context sizing and evidence.
// Unknown, remote, referenced, and binary documents remain media.
func IsTextDocument(b content.Block) bool {
	if b.Kind != content.KindDocument || b.Document == nil {
		return false
	}
	source := b.Document.Source.WithDetectedMIMEType(b.Document.Title)
	if len(source.Data) == 0 || !utf8.Valid(source.Data) || bytes.IndexByte(source.Data, 0) >= 0 {
		return false
	}
	switch source.MIMEType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml":
		return true
	}
	return strings.HasPrefix(source.MIMEType, "text/")
}

// HasMedia reports content whose encoded bytes do not establish context
// tokens, including content nested in local and provider tool results.
func HasMedia(messages []llm.Message) bool {
	has := func(blocks []content.Block) bool {
		return slices.ContainsFunc(blocks, func(b content.Block) bool {
			switch b.Kind {
			case content.KindImage, content.KindAudio, content.KindVideo:
				return true
			case content.KindDocument:
				return !IsTextDocument(b)
			}
			return false
		})
	}
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Block != nil && has([]content.Block{*part.Block}) ||
				part.ToolResult != nil && has(part.ToolResult.Output.Content) ||
				part.ProviderToolUse != nil && has(part.ProviderToolUse.Output) ||
				part.ProviderToolResult != nil && has(part.ProviderToolResult.Output) {
				return true
			}
		}
	}
	return false
}
