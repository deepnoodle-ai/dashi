package images

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

// Inline edit images may omit their MIME type: preparation detects it on a
// copy, uploads it with the detected type, and leaves the caller's input as
// given.
func TestEditDetectsUntypedImageMIMEType(t *testing.T) {
	client := testClient(
		t,
		"http://unused.invalid/v1",
		media.ImageCapabilities{
			Edit: model.SupportSupported,
			Mask: model.SupportSupported,
		},
	)
	input := imageInput(media.ImageEdit)
	image := content.ImageBlock(content.Source{Data: slices.Clone(tinyPNG)})
	input.ReferenceImages = []content.Block{image}
	input.Mask = &image
	request, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatalf("prepare edit with untyped bytes: %v", err)
	}
	if input.ReferenceImages[0].Image.Source.MIMEType != "" {
		t.Fatal("preparation changed the caller's source")
	}
	_, params, err := mime.ParseMediaType(request.ContentType())
	if err != nil {
		t.Fatal(err)
	}
	parts := multipart.NewReader(bytes.NewReader(request.Body()), params["boundary"])
	var files int
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() != "image" && part.FormName() != "mask" {
			continue
		}
		files++
		if got := part.Header.Get("Content-Type"); got != "image/png" {
			t.Fatalf("%s Content-Type = %q, want image/png", part.FormName(), got)
		}
		if part.FileName() != "image-0.png" {
			t.Fatalf("%s file name = %q", part.FormName(), part.FileName())
		}
	}
	if files != 2 {
		t.Fatalf("uploaded %d files, want image and mask", files)
	}
}

// A content reference names bytes that only a host can supply, so an edit
// gives an error that names the reference.
func TestEditRejectsContentReferenceByName(t *testing.T) {
	client := testClient(
		t,
		"http://unused.invalid/v1",
		media.ImageCapabilities{Edit: model.SupportSupported},
	)
	input := imageInput(media.ImageEdit)
	input.ReferenceImages = []content.Block{content.ImageBlock(content.Source{
		MIMEType: "image/png",
		Ref: &content.Ref{
			ID:     "upl_1",
			SHA256: strings.Repeat("0", 64),
			Size:   10,
		},
	})}

	_, err := client.PrepareImage(t.Context(), input)

	if err == nil || !strings.Contains(err.Error(), `content reference "upl_1"`) {
		t.Fatalf("error = %v", err)
	}
}
