package images

import (
	"testing"

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
