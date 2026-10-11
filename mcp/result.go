package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrInputRequired means the result needs host input before it can complete.
// Retain the original requests and request state in the application journal.
// Fulfill them under the host's input policy, or settle an unsupported-input
// failure. This error never authorizes a retry of completed tool execution.
var ErrInputRequired = errors.New("mcp: result requires host input")

// ConvertResult converts a completed SDK result without connecting to a server.
// Content keeps its order, and returned media bytes do not alias the result.
// PNG, JPEG, GIF, WebP, WAV and MP3 retain typed content after header validation;
// other image/audio types become omission notes and warnings. Resources become
// descriptions or omission notes, never fetched content. Provider support is
// separate from conversion.
//
// IsError produces a failed outcome. The host must also preserve journal failure
// and choose this converter from admitted tool identity, not JSON shape. On
// error the outcome is zero: retain the journal and settle known completed work
// as a safe failure through Pending.Result, without repeating execution.
// ErrInputRequired identifies a separate, incomplete host interaction.
//
// StructuredContent preserves the value supplied, including json.Number and
// json.RawMessage. It cannot restore numbers rounded before this call; use
// ConvertResultJSON for exact structured values from a saved JSON envelope.
// Callers must bound input size. Diagnostics never include payload values.
func ConvertResult(res *sdk.CallToolResult) (tool.Outcome, []string, error) {
	if res == nil {
		return tool.Outcome{}, nil, errors.New("mcp: result is required")
	}
	if res.NeedsInput() {
		return tool.Outcome{}, nil, ErrInputRequired
	}
	if res.InputRequests != nil || res.RequestState != "" {
		return tool.Outcome{}, nil, errors.New(
			"mcp: completed result excludes input requests and request state",
		)
	}
	var structured []byte
	if res.StructuredContent != nil {
		var err error
		structured, err = json.Marshal(res.StructuredContent)
		if err != nil {
			return tool.Outcome{}, nil, errors.New(
				"mcp: structured content requires a JSON value",
			)
		}
	}
	var blocks []content.Block
	var warnings []string
	if len(res.Meta) > 0 {
		warnings = append(warnings, "mcp: result metadata was not shown")
	}
	carried := false
	for i, c := range res.Content {
		block, lost, metadata, err := convertBlock(c)
		if err != nil {
			return tool.Outcome{}, warnings, fmt.Errorf(
				"mcp: content block %d: %w", i, err,
			)
		}
		blocks = append(blocks, block)
		if len(structured) > 0 && block.Text != nil {
			carried = carried || canonjson.Equal(
				[]byte(block.Text.Text), structured,
			)
		}
		if lost != "" {
			warnings = append(warnings, fmt.Sprintf(
				"mcp: content block %d: %s", i, lost,
			))
		}
		if metadata {
			warnings = append(warnings, fmt.Sprintf(
				"mcp: content block %d: metadata or annotations were not shown", i,
			))
		}
	}
	if len(structured) > 0 && !carried {
		blocks = append(blocks, content.TextBlock(string(structured)))
	}
	out := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: &tool.Output{Content: blocks},
	}
	if res.IsError {
		out.State = tool.StateFailed
	}
	if err := out.Validate(); err != nil {
		return tool.Outcome{}, warnings, errors.New(
			"mcp: result requires valid output content",
		)
	}
	return out, warnings, nil
}

func convertBlock(c sdk.Content) (content.Block, string, bool, error) {
	switch c := c.(type) {
	case *sdk.TextContent:
		if c != nil {
			return content.TextBlock(c.Text), "",
				len(c.Meta) > 0 || c.Annotations != nil, nil
		}
	case *sdk.ImageContent:
		if c != nil {
			block, lost, err := mediaBlock(c.Data, c.MIMEType, content.KindImage)
			return block, lost, len(c.Meta) > 0 || c.Annotations != nil, err
		}
	case *sdk.AudioContent:
		if c != nil {
			block, lost, err := mediaBlock(c.Data, c.MIMEType, content.KindAudio)
			return block, lost, len(c.Meta) > 0 || c.Annotations != nil, err
		}
	case *sdk.ResourceLink:
		if c != nil {
			if c.Name == "" || !validURI(c.URI) {
				return content.Block{}, "", false, errors.New(
					"resource link requires a name and absolute URI",
				)
			}
			if err := validResourceMIME(c.MIMEType); err != nil {
				return content.Block{}, "", false, err
			}
			if c.Size != nil && *c.Size < 0 {
				return content.Block{}, "", false, errors.New(
					"resource link size must be nonnegative",
				)
			}
			text := fmt.Sprintf("resource %s: %s", c.Name, c.URI)
			for _, description := range []string{
				c.Title, c.Description, c.MIMEType,
			} {
				if description != "" {
					text += "\n" + description
				}
			}
			if c.Size != nil {
				text += fmt.Sprintf("\n%d bytes", *c.Size)
			}
			metadata := len(c.Meta) > 0 || c.Annotations != nil || len(c.Icons) > 0
			return content.TextBlock(text), "", metadata, nil
		}
	case *sdk.EmbeddedResource:
		if c != nil {
			block, lost, err := embedded(c.Resource)
			metadata := len(c.Meta) > 0 || c.Annotations != nil ||
				(c.Resource != nil && len(c.Resource.Meta) > 0)
			return block, lost, metadata, err
		}
	case *sdk.ToolUseContent, *sdk.ToolResultContent:
		return content.Block{}, "", false, errors.New(
			"sampling content is not a tool result block",
		)
	default:
		if c != nil {
			return content.Block{}, "", false, errors.New(
				"unsupported tool result content",
			)
		}
	}
	return content.Block{}, "", false, errors.New("content entry is required")
}

func embedded(r *sdk.ResourceContents) (content.Block, string, error) {
	if r == nil || !validURI(r.URI) {
		return content.Block{}, "", errors.New("resource requires an absolute URI")
	}
	if err := validResourceMIME(r.MIMEType); err != nil {
		return content.Block{}, "", err
	}
	if r.Blob == nil {
		return content.TextBlock(fmt.Sprintf("resource %s:\n%s", r.URI, r.Text)),
			"", nil
	}
	if len(r.Blob) == 0 || r.Text != "" {
		return content.Block{}, "", errors.New(
			"binary resource requires nonempty bytes and excludes text",
		)
	}
	typ := r.MIMEType
	if typ == "" {
		typ = "unknown MIME type"
	}
	note := fmt.Sprintf(
		"binary resource %s (%s, %d bytes) was not shown",
		r.URI, typ, len(r.Blob),
	)
	return content.TextBlock(note), "binary resource was not shown", nil
}

func validURI(uri string) bool {
	u, err := url.Parse(uri)
	return err == nil && u.Scheme != "" && utf8.ValidString(uri)
}

func validResourceMIME(value string) error {
	if value != "" {
		if _, _, err := mime.ParseMediaType(value); err != nil {
			return errors.New("resource MIME type must be valid")
		}
	}
	return nil
}

func mediaBlock(
	data []byte,
	value string,
	kind content.Kind,
) (content.Block, string, error) {
	if len(data) == 0 {
		return content.Block{}, "", errors.New("media requires nonempty bytes")
	}
	typ, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.HasPrefix(typ, string(kind)+"/") {
		return content.Block{}, "", errors.New(
			"media MIME type must match its content kind",
		)
	}
	switch typ {
	case "image/jpg":
		typ = "image/jpeg"
	case "audio/wave", "audio/x-wav":
		typ = "audio/wav"
	case "audio/mp3":
		typ = "audio/mpeg"
	}
	valid, supported := mediaHeader(data, typ)
	if !supported {
		note := fmt.Sprintf(
			"%s content (%s, %d bytes) was not shown", kind, typ, len(data),
		)
		return content.TextBlock(note), "unsupported media format was not shown", nil
	}
	if !valid {
		return content.Block{}, "", errors.New(
			"media bytes require a header matching the MIME type",
		)
	}
	source := content.Source{Data: slices.Clone(data), MIMEType: typ}
	if kind == content.KindImage {
		return content.ImageBlock(source), "", nil
	}
	return content.AudioBlock(source), "", nil
}
