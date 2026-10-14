package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func screenshotResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "page loaded"},
		&mcp.ImageContent{Data: []byte{0xff, 0xd8, 0xff}, MIMEType: "Image/JPG"},
	}}
}

func checkScreenshot(t *testing.T, out tool.Outcome, warnings []string) {
	t.Helper()
	if out.State != tool.StateSucceeded || len(warnings) != 0 {
		t.Fatalf("state = %v, warnings = %v", out.State, warnings)
	}
	blocks := out.Output.Content
	if len(blocks) != 2 || blocks[0].Kind != content.KindText || blocks[1].Kind != content.KindImage {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].Text.Text != "page loaded" {
		t.Fatalf("text = %q", blocks[0].Text.Text)
	}
	src := blocks[1].Image.Source
	if src.MIMEType != "image/jpeg" || len(src.Data) != 3 {
		t.Fatalf("image = %+v", src)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConvertResultKeepsTextAndImage(t *testing.T) {
	out, warnings := ConvertResult(screenshotResult())
	checkScreenshot(t, out, warnings)
}

func TestConvertResultNotesEmptyMedia(t *testing.T) {
	out, warnings := ConvertResult(&mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{MIMEType: "image/png"},
		&mcp.AudioContent{Data: []byte{}, MIMEType: "audio/wav"},
	}})
	if err := out.Validate(); err != nil {
		t.Fatalf("outcome invalid: %v", err)
	}
	got := texts(out)
	if len(got) != 2 ||
		!strings.Contains(got[0], "empty image") ||
		!strings.Contains(got[1], "empty audio") {
		t.Fatalf("texts = %q", got)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q", warnings)
	}
}

func TestConvertResultAfterJSONRoundTrip(t *testing.T) {
	saved, err := json.Marshal(screenshotResult())
	if err != nil {
		t.Fatal(err)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(saved, &res); err != nil {
		t.Fatal(err)
	}
	out, warnings := ConvertResult(&res)
	checkScreenshot(t, out, warnings)
}

func TestConvertResultIsErrorFails(t *testing.T) {
	out, warnings := ConvertResult(&mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "it broke"}},
	})
	if out.State != tool.StateFailed || len(warnings) != 0 {
		t.Fatalf("state = %v, warnings = %v", out.State, warnings)
	}
	if got := out.Output.Content; len(got) != 1 || got[0].Text.Text != "it broke" {
		t.Fatalf("blocks = %+v", got)
	}
}

func TestConvertResultWarnsAboutUnknownContent(t *testing.T) {
	out, warnings := ConvertResult(&mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "before"},
		&mcp.ToolUseContent{ID: "x", Name: "y"},
	}})
	if out.State != tool.StateSucceeded {
		t.Fatalf("state = %v", out.State)
	}
	blocks := out.Output.Content
	if len(blocks) != 2 || !strings.Contains(blocks[1].Text.Text, "was not shown") {
		t.Fatalf("blocks = %+v", blocks)
	}
	if len(warnings) != 1 || warnings[0] != blocks[1].Text.Text {
		t.Fatalf("warnings = %v", warnings)
	}
}
