package images

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestComplexityRequiresGPTImage26(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-sunburst"
	fields, err := Options{Complexity: "low"}.fields(input)
	if err != nil || fields["complexity"] != "low" {
		t.Fatalf("complexity for 2.6: %#v, %v", fields, err)
	}
	input.Model = "future-image-model"
	if _, err := (Options{Complexity: "medium"}).fields(input); err != nil {
		t.Fatalf("unknown model complexity was rejected: %v", err)
	}
	input.Model = "gpt-image-2.5-sunburst"
	if _, err := (Options{Complexity: "high"}).fields(input); err == nil {
		t.Fatal("GPT Image 2.5 accepted complexity")
	}
	input.Model = "gpt-image-2.6-sunburst"
	if _, err := (Options{Complexity: "max"}).fields(input); err == nil {
		t.Fatal("invalid complexity was accepted")
	}
}

func TestGPTImage1MiniAcceptsOnlyLowFidelity(t *testing.T) {
	input := imageInput(media.ImageEdit)
	input.Model = "gpt-image-1-mini"
	if _, err := (Options{InputFidelity: "high"}).fields(input); err == nil {
		t.Fatal("gpt-image-1-mini accepted high input fidelity")
	}
	fields, err := Options{InputFidelity: "low"}.fields(input)
	if err != nil || fields["input_fidelity"] != "low" {
		t.Fatalf("low fidelity: %#v, %v", fields, err)
	}
}

func intPtr(v int) *int { return &v }

func TestGIFFrameOptions(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	input.Settings.OutputMIMEType = "image/gif"
	fields, err := Options{
		FrameCount:      intPtr(4),
		FrameDurationMS: intPtr(120),
	}.fields(input)
	if err != nil || fields["frame_count"] != 4 ||
		fields["frame_duration_ms"] != 120 {
		t.Fatalf("GIF frame fields: %#v, %v", fields, err)
	}
	for name, options := range map[string]Options{
		"too few frames":  {FrameCount: intPtr(1)},
		"too many frames": {FrameCount: intPtr(17)},
		"short frame":     {FrameDurationMS: intPtr(10)},
	} {
		if _, err := options.fields(input); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	input.Settings.OutputMIMEType = "image/png"
	if _, err := (Options{FrameCount: intPtr(4)}).fields(input); err == nil {
		t.Error("frame count without GIF output was accepted")
	}
	if _, err := (Options{FrameDurationMS: intPtr(100)}).fields(input); err == nil {
		t.Error("frame duration without GIF output was accepted")
	}
}

func TestSVGOptions(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	input.Settings.OutputMIMEType = "image/svg+xml"
	fields, err := Options{
		SVG: &SVGOptions{Model: "gpt-5.5", ReasoningEffort: "low"},
	}.fields(input)
	if err != nil {
		t.Fatal(err)
	}
	svg, _ := fields["svg"].(map[string]any)
	reasoning, _ := svg["reasoning"].(map[string]any)
	if svg["model"] != "gpt-5.5" || reasoning["effort"] != "low" {
		t.Fatalf("svg field: %#v", fields["svg"])
	}
	fields, err = Options{SVG: &SVGOptions{Model: "gpt-5.5"}}.fields(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["svg"].(map[string]any)["reasoning"]; ok {
		t.Fatal("empty reasoning effort was sent")
	}
	for name, options := range map[string]Options{
		"no SVG options":  {},
		"no author model": {SVG: &SVGOptions{}},
		"bad effort":      {SVG: &SVGOptions{Model: "gpt-5.5", ReasoningEffort: "huge"}},
	} {
		if _, err := options.fields(input); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	input.Settings.OutputMIMEType = "image/png"
	if _, err := (Options{SVG: &SVGOptions{Model: "gpt-5.5"}}).fields(input); err == nil {
		t.Error("SVG options without SVG output were accepted")
	}
}

func TestTransparentBackgroundRequiresPNGOrWebP(t *testing.T) {
	input := imageInput(media.ImageGenerate)
	input.Model = "gpt-image-2.6-flare"
	for _, mimeType := range []string{"image/jpeg", "image/gif", "image/svg+xml"} {
		input.Settings.OutputMIMEType = mimeType
		if _, err := (Options{Background: "transparent"}).fields(input); err == nil {
			t.Errorf("transparent background with %s was accepted", mimeType)
		}
	}
	input.Settings.OutputMIMEType = "image/webp"
	if _, err := (Options{Background: "transparent"}).fields(input); err != nil {
		t.Errorf("transparent WebP: %v", err)
	}
}

func TestMarkupRules(t *testing.T) {
	markup := content.ImageBlock(content.Source{Data: tinyPNG, MIMEType: "image/png"})
	input := imageInput(media.ImageEdit)
	input.Model = "gpt-image-2.5-sunburst"
	input.ReferenceImages = []content.Block{markup}
	fields, err := Options{Markup: &markup}.fields(input)
	ref, _ := fields["markup"].(map[string]string)
	if err != nil || !strings.HasPrefix(ref["image_url"], "data:image/png;base64,") {
		t.Fatalf("markup field: %#v, %v", fields, err)
	}
	input.Model = "gpt-image-2"
	if _, err := (Options{Markup: &markup}).fields(input); err == nil {
		t.Error("GPT Image 2 accepted markup")
	}
	input.Model = "gpt-image-2.6-flare"
	input.Mask = &markup
	if _, err := (Options{Markup: &markup}).fields(input); err == nil {
		t.Error("markup with a mask was accepted")
	}
	input.Mask = nil
	jpeg := content.ImageBlock(content.Source{
		Data:     []byte("\xff\xd8\xff\xe0jpeg"),
		MIMEType: "image/jpeg",
	})
	if _, err := (Options{Markup: &jpeg}).fields(input); err == nil {
		t.Error("JPEG markup was accepted")
	}
	file := content.ImageBlock(content.Source{ID: "file_123"})
	if _, err := (Options{Markup: &file}).fields(input); err == nil {
		t.Error("markup file ID without a Files scope was accepted")
	}
	generate := imageInput(media.ImageGenerate)
	generate.Model = "gpt-image-2.6-flare"
	if _, err := (Options{Markup: &markup}).fields(generate); err == nil {
		t.Error("markup on a generation was accepted")
	}
}

func TestCloneCopiesOptionPointers(t *testing.T) {
	markup := content.ImageBlock(content.Source{Data: []byte{1, 2}, MIMEType: "image/png"})
	original := Options{
		FrameCount:      intPtr(3),
		FrameDurationMS: intPtr(50),
		SVG:             &SVGOptions{Model: "a"},
		Markup:          &markup,
	}
	clone := original.clone()
	*original.FrameCount, *original.FrameDurationMS = 9, 99
	original.SVG.Model = "b"
	original.Markup.Image.Source.Data[0] = 9
	if *clone.FrameCount != 3 || *clone.FrameDurationMS != 50 ||
		clone.SVG.Model != "a" || clone.Markup.Image.Source.Data[0] != 1 {
		t.Fatalf("clone shares memory: %#v", clone)
	}
}
