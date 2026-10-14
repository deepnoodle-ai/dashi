package images

import (
	"bytes"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

func TestMultipartBodyIsSameForSameInput(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := imageInput(media.ImageEdit)
	input.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"}),
	}
	first, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Body(), second.Body()) ||
		first.ContentType() != second.ContentType() {
		t.Fatal("same input prepared different multipart bodies")
	}
}
