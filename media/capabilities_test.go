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

func TestVideoCapabilitiesCheckDistinguishesOperations(t *testing.T) {
	input := VideoInput{Model: "video-model", Operation: VideoEdit}
	capabilities := VideoCapabilities{
		Generate: model.SupportSupported,
		Edit:     model.SupportUnsupported,
		Extend:   model.SupportUnknown,
	}
	if _, err := capabilities.Check(input, model.UnknownAllow); err == nil {
		t.Fatal("allowed known unsupported video edit")
	}
	input.Operation = VideoExtend
	unverified, err := capabilities.Check(input, model.UnknownAllow)
	if err != nil || !slices.Equal(unverified, []string{"video.extend"}) {
		t.Fatalf("unknown extend support = %v, %v", unverified, err)
	}
	input.Operation = ""
	if _, err := capabilities.Check(input, model.UnknownReject); err != nil {
		t.Fatalf("zero operation did not default to generate: %v", err)
	}
}

func TestImageSizeBoundsCheckFlexibleDimensions(t *testing.T) {
	bounds := ImageSizeBounds{
		MinPixels: 655_360, MaxPixels: 8_294_400,
		MaxEdge: 3840, Multiple: 16, MaxAspectRatio: 3,
	}
	for _, size := range []Size{{1024, 1024}, {3840, 2160}, {1536, 1024}} {
		if err := bounds.Check(size); err != nil {
			t.Fatalf("accepted size %+v rejected: %v", size, err)
		}
	}
	for _, size := range []Size{{512, 512}, {3856, 1024}, {1025, 1024}, {3840, 1024}} {
		if err := bounds.Check(size); err == nil {
			t.Fatalf("unsupported size %+v accepted", size)
		}
	}
	input := ImageInput{Model: "image-model", Operation: ImageGenerate,
		Prompt:   []content.Block{content.TextBlock("a mountain")},
		Settings: ImageSettings{Size: Size{1024, 1024}}}
	capabilities := ImageCapabilities{Generate: model.SupportSupported, SizeBounds: &bounds}
	if _, err := capabilities.Check(input, model.UnknownReject); err != nil {
		t.Fatalf("strict evidence rejected supported size: %v", err)
	}
	input.Settings.Size = Size{512, 512}
	if _, err := capabilities.Check(input, model.UnknownAllow); err == nil {
		t.Fatal("known unsupported size was allowed")
	}
}
