package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// convertResult maps an MCP result to a Dashi outcome. Blocks keep their
// order and nothing vanishes silently: content that the model cannot use
// becomes a text note, and the note text goes to warn.
func convertResult(res *mcp.CallToolResult, warn func(string)) tool.Outcome {
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
				MIMEType: c.MIMEType,
			}))
		case *mcp.AudioContent:
			blocks = append(blocks, content.AudioBlock(content.Source{
				Data:     c.Data,
				MIMEType: c.MIMEType,
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
	return outcome
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
