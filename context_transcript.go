package dashi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/compactiontext"
	"github.com/deepnoodle-ai/dashi/internal/llmclone"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Use one qualification rule for context sizing and summary evidence.
// Unknown, remote, and binary documents keep unavailable sizing and markers.
func activeTextDocument(b content.Block) bool {
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

// activeTranscript renders documents only on a transcript copy. References
// reuse the ordinary candidate's verified bytes; rendering performs no I/O.
// Between-Turn compaction retains its existing transcript behavior.
func (r *runtime) activeTranscript(messages []llm.Message) ([]string, error) {
	messages = llmclone.Messages(messages)
	var verified map[string][]byte
	for _, message := range messages {
		for _, part := range message.Content {
			for _, source := range contentSources(part) {
				if source.Ref == nil {
					continue
				}
				if verified == nil {
					verified = map[string][]byte{}
					if r.compactionCandidate != nil {
						for _, m := range r.compactionCandidate.Messages {
							for _, p := range m.Content {
								for _, s := range contentSources(p) {
									if s.Data != nil {
										sum := sha256.Sum256(s.Data)
										verified[hex.EncodeToString(sum[:])] = s.Data
									}
								}
							}
						}
					}
				}
				data, ok := verified[source.Ref.SHA256]
				if !ok || int64(len(data)) != source.Ref.Size {
					return nil, errors.New("summary evidence lacks verified reference bytes")
				}
				source.Data = data
				source.Ref = nil
			}
		}
	}
	textBlock := func(b content.Block) content.Block {
		if activeTextDocument(b) {
			return content.TextBlock("[text document: " + b.Document.Title + "]\n" + string(b.Document.Source.Data))
		}
		return b
	}
	render := func(blocks []content.Block) {
		for i, b := range blocks {
			blocks[i] = textBlock(b)
		}
	}
	providerToolOutput := func(blocks []content.Block, isError bool) []content.Block {
		render(blocks)
		if isError {
			// The shared renderer does not label provider tool errors. Keep
			// explicit failure status visible in this active-only evidence
			// copy.
			return append([]content.Block{content.TextBlock("[provider tool error]")}, blocks...)
		}
		return blocks
	}
	for i, message := range messages {
		var parts []llm.Content
		for _, part := range message.Content {
			if part.Block != nil {
				*part.Block = textBlock(*part.Block)
			}
			if part.ToolResult != nil {
				render(part.ToolResult.Output.Content)
			}
			if part.ProviderToolResult != nil {
				part.ProviderToolResult.Output = providerToolOutput(part.ProviderToolResult.Output, part.ProviderToolResult.IsError)
			}
			parts = append(parts, part)
			if use := part.ProviderToolUse; use != nil && (len(use.Output) > 0 || use.IsError) {
				output := providerToolOutput(use.Output, use.IsError)
				parts = append(parts, llm.Content{Kind: llm.ContentProviderToolResult, ProviderToolResult: &llm.ProviderToolResult{
					CallID: use.ID, Type: use.Type, Origin: use.Origin, Output: output, IsError: use.IsError,
				}})
			}
		}
		messages[i].Content = parts
	}
	return compactiontext.Transcript(messages), nil
}
