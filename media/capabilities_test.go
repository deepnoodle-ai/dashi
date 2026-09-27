package media

import (
	"slices"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

func TestImageCapabilitiesCheckSeparatesUnknownFromUnsupported(t *testing.T) {
	input := ImageInput{
		Model:     "image-model",
		Operation: ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("a mountain")},
		Settings:  ImageSettings{Resolution: "2k"},
	}
	capabilities := ImageCapabilities{Generate: model.SupportSupported}
	if _, err := capabilities.Check(input, model.UnknownReject); err == nil {
		t.Fatal("strict preparation accepted unknown resolution support")
	}
	unverified, err := capabilities.Check(input, model.UnknownAllow)
	if err != nil {
		t.Fatalf("allowed unknown resolution rejected: %v", err)
	}
	if !slices.Equal(unverified, []string{"image.resolution"}) {
		t.Fatalf("unverified=%v, want image.resolution", unverified)
	}
	capabilities.Resolutions = []string{"1k"}
	if _, err := capabilities.Check(input, model.UnknownAllow); err == nil {
		t.Fatal("known unsupported resolution was allowed")
	}
}

func TestVideoCapabilitiesCheckAcceptsKnownDurationRange(t *testing.T) {
	input := VideoInput{
		Model:  "video-model",
		Prompt: []content.Block{content.TextBlock("a mountain")},
		Settings: VideoSettings{
			Duration: 8 * time.Second,
		},
	}
	capabilities := VideoCapabilities{
		Generate:    model.SupportSupported,
		MinDuration: time.Second,
		MaxDuration: 15 * time.Second,
	}
	unverified, err := capabilities.Check(input, model.UnknownReject)
	if err != nil {
		t.Fatalf("known video duration range rejected: %v", err)
	}
	if len(unverified) != 0 {
		t.Fatalf("known capability reported unverified: %v", unverified)
	}
	input.Settings.Duration = 16 * time.Second
	if _, err := capabilities.Check(input, model.UnknownAllow); err == nil {
		t.Fatal("video duration above known limit was allowed")
	}
}
