package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestNanoBanana21ImageLimits(t *testing.T) {
	const name = "gemini-nano-banana-2.1"
	if err := validateImageModelResolution(name, "512"); err == nil {
		t.Fatal("gemini-nano-banana-2.1 accepted 512")
	}
	for _, resolution := range []string{"1K", "2K", "4K"} {
		if err := validateImageModelResolution(name, resolution); err != nil {
			t.Errorf("resolution %s: %v", resolution, err)
		}
	}
	for _, ratio := range []string{"21:9", "1:8", "8:1"} {
		if err := validateImageModelAspectRatio(name, ratio); err != nil {
			t.Errorf("aspect ratio %s: %v", ratio, err)
		}
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	references := make([]content.Block, 15)
	for i := range references {
		references[i] = content.ImageBlock(content.Source{
			Data:     []byte("png"),
			MIMEType: "image/png",
		})
	}
	if _, err := p.PrepareImage(t.Context(), media.ImageInput{
		Model:           name,
		Operation:       media.ImageEdit,
		Prompt:          []content.Block{content.TextBlock("combine")},
		ReferenceImages: references,
	}); err == nil {
		t.Fatal("gemini-nano-banana-2.1 accepted 15 reference images")
	}
}

func TestInteractionsImageErrorIncludesModelText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"i-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"I can't draw that."}]}]}`))
	}))
	defer server.Close()
	p := newInteractionsFixture(t, server)
	request, err := p.PrepareImage(t.Context(), media.ImageInput{
		Model:     "gemini-nano-banana-2.1",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("a red circle")},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.SendImage(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "no image") ||
		!strings.Contains(err.Error(), "I can't draw that.") {
		t.Fatalf("error = %v", err)
	}
}
