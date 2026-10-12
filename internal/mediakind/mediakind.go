// Package mediakind decides which content blocks are media: content whose
// encoded bytes do not show how many tokens it takes. Context sizing,
// compaction evidence, and the input token estimate share this one rule.
package mediakind

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
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

// IsMedia reports an image, audio, or video block, or a document that is
// not a text document.
func IsMedia(b content.Block) bool {
	switch b.Kind {
	case content.KindImage, content.KindAudio, content.KindVideo:
		return true
	case content.KindDocument:
		return !IsTextDocument(b)
	}
	return false
}
