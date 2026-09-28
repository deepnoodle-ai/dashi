package llm

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
)

func TestImageAndDocumentConstructors(t *testing.T) {
	image := Image(content.Source{URL: "https://example.com/a.png"})
	if image.Kind != ContentBlock || image.Block.Image == nil ||
		image.Block.Image.Source.URL != "https://example.com/a.png" {
		t.Fatalf("image = %+v", image)
	}
	doc := Document(content.Source{Data: []byte("text")}, "notes.txt")
	if doc.Block.Document == nil || doc.Block.Document.Title != "notes.txt" {
		t.Fatalf("document = %+v", doc)
	}
	// Inline bytes without a MIME type are valid; preparation detects it.
	if err := UserMessage(image, doc).Content[1].Validate(); err != nil {
		t.Fatalf("untyped inline document: %v", err)
	}
}
