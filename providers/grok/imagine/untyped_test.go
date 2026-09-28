package imagine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

// Inline edit images may omit their MIME type: preparation detects it on a
// copy, so the data URL names a real type, and leaves the caller's input as
// given.
func TestEditDetectsUntypedImageMIMEType(t *testing.T) {
	client := testClient(
		t,
		"http://unused.invalid/v1",
		nil,
		model.CapabilityAdjust,
	)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageEdit,
		Prompt:    []content.Block{content.TextBlock("Make it blue")},
		ReferenceImages: []content.Block{
			content.ImageBlock(content.Source{Data: png}),
		},
	}
	request, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatalf("prepare edit with untyped bytes: %v", err)
	}
	if input.ReferenceImages[0].Image.Source.MIMEType != "" {
		t.Fatal("preparation changed the caller's source")
	}
	var wire struct {
		Image map[string]string `json:"image"`
	}
	if err := json.Unmarshal(request.Body(), &wire); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wire.Image["url"], "data:image/png;base64,") {
		t.Fatalf("image url = %.40q", wire.Image["url"])
	}
}
