package images

import (
	"bytes"
	"encoding/json"
	"strings"
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

func TestGPTImage26UsesCurrentImage2Limits(t *testing.T) {
	for _, name := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		if !validSize(name, media.Size{Width: 3840, Height: 2160}) ||
			validSize(name, media.Size{Width: 3840, Height: 1024}) {
			t.Errorf("%s does not use the flexible size bounds", name)
		}
		for _, quality := range []string{"low", "high", "xhigh", "max"} {
			if !validQuality(name, quality) {
				t.Errorf("%s rejected quality %q", name, quality)
			}
		}
		if validQuality(name, "hd") {
			t.Errorf("%s accepted DALL-E quality hd", name)
		}
	}
}

func TestFixedSizeModelsIncludeDatedGPTImage15(t *testing.T) {
	for _, name := range []string{
		"gpt-image-1-mini",
		"gpt-image-1.5",
		"gpt-image-1.5-2025-12-16",
	} {
		if !validSize(name, media.Size{Width: 1536, Height: 1024}) ||
			validSize(name, media.Size{Width: 1536, Height: 864}) {
			t.Errorf("%s does not use the three fixed sizes", name)
		}
		if validQuality(name, "max") {
			t.Errorf("%s accepted max quality", name)
		}
	}
}

func TestRetiredGPTImage1IsRejected(t *testing.T) {
	err := validateModelOperation("gpt-image-1", media.ImageGenerate)
	if err == nil || !strings.Contains(err.Error(), "2026-10-23") {
		t.Fatalf("gpt-image-1 error = %v", err)
	}
	if err := validateModelOperation("gpt-image-1-mini", media.ImageGenerate); err != nil {
		t.Fatalf("gpt-image-1-mini is served until 2026-12-01: %v", err)
	}
}

func TestGIFOutputRequiresGPTImage26(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Settings.OutputMIMEType = "image/gif"
	input.Model = "gpt-image-2.6-flare"
	fields, err := imageFields(input, "a spinning top")
	if err != nil || fields["output_format"] != "gif" {
		t.Fatalf("GIF output for 2.6: %#v, %v", fields, err)
	}
	input.Model = "future-image-model"
	if _, err := imageFields(input, "a spinning top"); err != nil {
		t.Fatalf("unknown model GIF output was rejected: %v", err)
	}
	input.Model = "gpt-image-2.5-flare"
	if _, err := imageFields(input, "a spinning top"); err == nil {
		t.Fatal("GPT Image 2.5 accepted GIF output")
	}
	edit := imageInput(media.ImageEdit)
	edit.Model = "gpt-image-2.6-sunburst"
	edit.Settings.OutputMIMEType = "image/gif"
	edit.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"}),
	}
	mask := content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"})
	edit.Mask = &mask
	if _, err := imageFields(edit, "a spinning top"); err == nil {
		t.Fatal("GIF output accepted a mask")
	}
}

func TestSVGOutputRules(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	input.Settings.OutputMIMEType = "image/svg+xml"
	input.Settings.Count = 1
	fields, err := imageFields(input, "a logo")
	if err != nil || fields["output_format"] != "svg" {
		t.Fatalf("SVG generation: %#v, %v", fields, err)
	}
	input.Settings.Count = 2
	if _, err := imageFields(input, "a logo"); err == nil {
		t.Error("SVG output accepted a count of two")
	}
	input.Settings.Count = 1
	input.Model = "gpt-image-2.5-flare"
	if _, err := imageFields(input, "a logo"); err == nil {
		t.Error("GPT Image 2.5 accepted SVG output")
	}
	input.Model = "future-image-model"
	if _, err := imageFields(input, "a logo"); err != nil {
		t.Errorf("unknown model SVG output was rejected: %v", err)
	}
	edit := imageInput(media.ImageEdit)
	edit.Model = "gpt-image-2.6-flare"
	edit.Settings.OutputMIMEType = "image/svg+xml"
	edit.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"}),
	}
	if _, err := imageFields(edit, "a logo"); err != nil {
		t.Errorf("GPT Image 2.6 SVG edit: %v", err)
	}
	mask := content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"})
	edit.Mask = &mask
	if _, err := imageFields(edit, "a logo"); err == nil {
		t.Error("SVG edit accepted a mask")
	}
	compression := 50
	if _, err := (Options{OutputCompression: &compression}).fields(input); err == nil {
		t.Error("SVG output accepted output compression")
	}
}

func TestMarkupEditUsesJSONBody(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := imageInput(media.ImageEdit)
	input.Model = "gpt-image-2.6-sunburst"
	input.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"}),
	}
	markup := content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"})
	request, err := client.PrepareImageWithOptions(
		t.Context(),
		input,
		Options{Markup: &markup},
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ContentType() != "application/json" {
		t.Fatalf("content type %q", request.ContentType())
	}
	var body struct {
		Images []map[string]string `json:"images"`
		Markup map[string]string   `json:"markup"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Images) != 1 ||
		!strings.HasPrefix(body.Markup["image_url"], "data:image/png;base64,") {
		t.Fatalf("markup body: %s", request.Body())
	}
}

func TestSVGGenerationBody(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	input.Settings.OutputMIMEType = "image/svg+xml"
	request, err := client.PrepareImageWithOptions(
		t.Context(),
		input,
		Options{SVG: &SVGOptions{Model: "gpt-5.5", ReasoningEffort: "minimal"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := `"svg":{"model":"gpt-5.5","reasoning":{"effort":"minimal"}}`
	if !strings.Contains(string(request.Body()), want) ||
		!strings.Contains(string(request.Body()), `"output_format":"svg"`) ||
		!strings.Contains(string(request.Body()), `"n":1`) {
		t.Fatalf("SVG body: %s", request.Body())
	}
}
