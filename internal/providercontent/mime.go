package providercontent

import (
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

// detectMIMETypes fills the MIME type of inline bytes that have none, in c and
// in a tool result's blocks. c must already be an owned copy. It returns the
// reason for one detection change covering them all, empty when nothing was
// detected, and whether any type stayed unknown.
func detectMIMETypes(c *llm.Content) (reason string, unknown bool) {
	var blocks []content.Block
	switch {
	case c.Block != nil:
		blocks = []content.Block{*c.Block}
	case c.ToolResult != nil:
		blocks = c.ToolResult.Output.Content
	default:
		return "", false
	}
	var detected []string
	for i := range blocks {
		source, name := untypedInline(&blocks[i])
		if source == nil {
			continue
		}
		source.MIMEType = content.DetectMIMEType(source.Data, name)
		unknown = unknown || source.MIMEType == content.UnknownMIMEType
		label := string(blocks[i].Kind)
		if c.ToolResult != nil {
			label = fmt.Sprintf("tool result block %d (%s)", i, label)
		}
		detected = append(detected, label+" as "+source.MIMEType)
	}
	if c.Block != nil {
		*c.Block = blocks[0]
	}
	if len(detected) == 0 {
		return "", false
	}
	return "inline data has no MIME type; detected " +
		strings.Join(detected, ", "), unknown
}

// untypedInline returns the source of b when it holds inline bytes without a
// MIME type, with a document's title as a file name hint.
func untypedInline(b *content.Block) (*content.Source, string) {
	var source *content.Source
	name := ""
	switch {
	case b.Image != nil:
		source = &b.Image.Source
	case b.Document != nil:
		source = &b.Document.Source
		name = b.Document.Title
	case b.Audio != nil:
		source = &b.Audio.Source
	case b.Video != nil:
		source = &b.Video.Source
	}
	if source == nil || source.Data == nil || source.MIMEType != "" {
		return nil, ""
	}
	return source, name
}
