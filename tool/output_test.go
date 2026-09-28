package tool

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func TestOutputMapTextKeepsOtherBlocks(t *testing.T) {
	image := content.ImageBlock(content.Source{Data: []byte{1, 2}, MIMEType: "image/png"})
	in := Output{
		Content: []content.Block{content.TextBlock("key=sk-1"), image},
		Display: "shown",
	}
	out := in.MapText(func(s string) string {
		return strings.ReplaceAll(s, "sk-1", "[redacted]")
	})
	if out.Content[0].Text.Text != "key=[redacted]" || out.Display != "shown" {
		t.Fatalf("out = %+v", out)
	}
	if out.Content[1].Kind != content.KindImage || out.Content[1].Image == nil {
		t.Fatalf("image lost: %+v", out.Content[1])
	}
	if in.Content[0].Text.Text != "key=sk-1" {
		t.Fatal("MapText changed its input")
	}
}
