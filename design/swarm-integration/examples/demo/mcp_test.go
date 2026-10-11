// toolResultOutcome is copied from Swarm a41b9a4, dashirt.go:956-994.
// The only change is a local substitute for its internal ToolResult type.
package demo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
)

type journalResult struct {
	Output  json.RawMessage
	IsError bool
}

func toolResultOutcome(res journalResult) tool.Outcome {
	output := tool.TextOutput(string(res.Output))
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Data string `json:"data"`
		MIME string `json:"mimeType"`
	}
	if json.Unmarshal(res.Output, &blocks) == nil && len(blocks) > 0 {
		var typed []content.Block
		valid := true
		for _, block := range blocks {
			switch block.Type {
			case "text":
				typed = append(typed, content.TextBlock(block.Text))
			case "image":
				data, err := base64.StdEncoding.DecodeString(block.Data)
				if err != nil || len(data) == 0 || !strings.HasPrefix(block.MIME, "image/") {
					valid = false
					break
				}
				typed = append(typed, content.ImageBlock(content.Source{Data: data, MIMEType: block.MIME}))
			default:
				valid = false
			}
			if !valid {
				break
			}
		}
		if valid {
			output = tool.Output{Content: typed}
		}
	}
	state := tool.StateSucceeded
	if res.IsError {
		state = tool.StateFailed
	}
	return tool.Outcome{State: state, Output: &output}
}

// unwrapMCP is a LOCAL DEMO of the array/object mismatch, not the proposed
// public converter. It covers this fixture only and lacks Dashi's full MCP
// audio/resource/MIME/structured-content handling.
func unwrapMCP(raw json.RawMessage) (tool.Outcome, error) {
	var envelope struct {
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return tool.Outcome{}, err
	}
	return toolResultOutcome(journalResult{Output: envelope.Content, IsError: envelope.IsError}), nil
}

func Example_mcpImage() {
	var pixel bytes.Buffer
	if err := png.Encode(&pixel, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		panic(err)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":"Screenshot"},{"type":"image","data":%q,"mimeType":"image/png"}],"isError":false}`, base64.StdEncoding.EncodeToString(pixel.Bytes())))
	before := toolResultOutcome(journalResult{Output: raw})
	after, err := unwrapMCP(raw)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Swarm today: %d blocks, first kind: %s\n", len(before.Output.Content), before.Output.Content[0].Kind)
	fmt.Printf("local envelope demo: %d blocks, last kind: %s\n", len(after.Output.Content), after.Output.Content[1].Kind)
	// Output:
	// Swarm today: 1 blocks, first kind: text
	// local envelope demo: 2 blocks, last kind: image
}
