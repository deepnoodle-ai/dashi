package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConvertResult maps an MCP tool result to a Dashi outcome, the same way
// Toolset does. Use it when the application calls MCP tools itself, for
// example to replay a result it saved with json.Marshal. Blocks keep their
// order and nothing vanishes silently: content the model cannot use becomes a
// text note, and each note is also returned as a warning. A result with
// IsError is a failed outcome. A nil result is an empty succeeded outcome.
func ConvertResult(res *mcp.CallToolResult) (tool.Outcome, []string) {
	var warnings []string
	warn := func(text string) { warnings = append(warnings, text) }
	if res == nil {
		res = &mcp.CallToolResult{}
	}
	var blocks []content.Block
	carried := false
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			blocks = append(blocks, content.TextBlock(c.Text))
			carried = carried || sameJSON(c.Text, res.StructuredContent)
		case *mcp.ImageContent:
			blocks = append(blocks, content.ImageBlock(content.Source{
				Data:     c.Data,
				MIMEType: mimeType(c.MIMEType),
			}))
		case *mcp.AudioContent:
			blocks = append(blocks, content.AudioBlock(content.Source{
				Data:     c.Data,
				MIMEType: mimeType(c.MIMEType),
			}))
		case *mcp.ResourceLink:
			blocks = append(blocks, content.TextBlock(
				fmt.Sprintf("resource %s: %s", c.Name, c.URI),
			))
		case *mcp.EmbeddedResource:
			blocks = append(blocks, embedded(c.Resource, warn))
		default:
			note := fmt.Sprintf("content of type %T was not shown", c)
			warn(note)
			blocks = append(blocks, content.TextBlock(note))
		}
	}
	if res.StructuredContent != nil && !carried {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			blocks = append(blocks, content.TextBlock(string(raw)))
		}
	}
	outcome := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: &tool.Output{Content: blocks},
	}
	if res.IsError {
		outcome.State = tool.StateFailed
	}
	return outcome, warnings
}

// mimeType lowercases a media type and maps the common image/jpg alias to
// image/jpeg, which is what model providers accept.
func mimeType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "image/jpg" {
		return "image/jpeg"
	}
	return s
}

func embedded(r *mcp.ResourceContents, warn func(string)) content.Block {
	if r == nil {
		return content.TextBlock("empty resource")
	}
	if len(r.Blob) == 0 {
		return content.TextBlock(fmt.Sprintf("resource %s:\n%s", r.URI, r.Text))
	}
	note := fmt.Sprintf(
		"binary resource %s (%s, %d bytes) was not shown",
		r.URI,
		r.MIMEType,
		len(r.Blob),
	)
	warn(note)
	return content.TextBlock(note)
}

// sameJSON reports whether text is JSON with the same value as structured.
func sameJSON(text string, structured any) bool {
	if structured == nil {
		return false
	}
	var a, b any
	raw, err := json.Marshal(structured)
	if err != nil || json.Unmarshal(raw, &b) != nil {
		return false
	}
	return json.Unmarshal([]byte(text), &a) == nil && reflect.DeepEqual(a, b)
}
