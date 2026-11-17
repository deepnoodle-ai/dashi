package imagine

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestVideoLiteAccepts1080pAndSilence(t *testing.T) {
	input := media.VideoInput{
		Model:  "grok-imagine-video-1.5-lite",
		Prompt: []content.Block{content.TextBlock("A lake")},
		Settings: media.VideoSettings{
			Resolution:   "1080p",
			DisableAudio: true,
		},
	}
	raw, err := videoWireBody(input)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["resolution"] != "1080p" || body["generate_audio"] != false {
		t.Fatalf("lite body = %s", raw)
	}
	input.Model = "grok-imagine-video"
	input.Settings.DisableAudio = false
	if _, err := videoWireBody(input); err == nil {
		t.Fatal("grok-imagine-video accepted 1080p")
	}
}

func TestVideoAcceptsWideAspectRatios(t *testing.T) {
	for _, ratio := range []string{"21:9", "5:2"} {
		input := media.VideoInput{
			Model:    "grok-imagine-video-1.5",
			Prompt:   []content.Block{content.TextBlock("A lake")},
			Settings: media.VideoSettings{AspectRatio: ratio},
		}
		if _, err := videoWireBody(input); err != nil {
			t.Errorf("aspect ratio %s: %v", ratio, err)
		}
	}
}

func TestVideo15AcceptsFourteenReferenceImages(t *testing.T) {
	references := make([]content.Block, 14)
	for i := range references {
		references[i] = content.ImageBlock(content.Source{
			URL: "https://example.test/guide.png",
		})
	}
	input := media.VideoInput{
		Model:  "grok-imagine-video-1.5",
		Prompt: []content.Block{content.TextBlock("A lake")},
	}
	if _, err := videoWireBodyWithOptions(
		input,
		VideoOptions{ReferenceImages: references},
	); err != nil {
		t.Fatal(err)
	}
}
