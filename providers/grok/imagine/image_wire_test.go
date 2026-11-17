package imagine

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestImageWideAspectRatiosRequireImage2(t *testing.T) {
	for _, ratio := range []string{"21:9", "5:2"} {
		input := media.ImageInput{
			Model:     "grok-imagine-image-2.0",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("A lake")},
			Settings:  media.ImageSettings{AspectRatio: ratio},
		}
		if _, _, err := imageWireBody(input); err != nil {
			t.Errorf("2.0 aspect ratio %s: %v", ratio, err)
		}
		for _, name := range []string{
			"grok-imagine-image",
			"grok-imagine-image-2026-03-02",
		} {
			input.Model = name
			if _, _, err := imageWireBody(input); err == nil {
				t.Errorf("%s accepted aspect ratio %s", name, ratio)
			}
		}
	}
}

func TestImage2ControlsFollowTheImage2Family(t *testing.T) {
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.1",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("A lake")},
		Settings:  media.ImageSettings{Quality: "medium", Resolution: "2k"},
	}
	if _, _, err := imageWireBody(input); err != nil {
		t.Fatalf("later 2.x model rejected 2.0 controls: %v", err)
	}
	input.Model = "grok-imagine-image"
	if _, _, err := imageWireBody(input); err == nil {
		t.Fatal("grok-imagine-image accepted quality and resolution")
	}
}
