package imagine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
)

func TestImageEditUsesJSONDataURL(t *testing.T) {
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageEdit,
		Prompt:    []content.Block{content.TextBlock("Make it blue")},
		ReferenceImages: []content.Block{
			content.ImageBlock(content.Source{
				Data:     []byte("image bytes"),
				MIMEType: "image/png",
			}),
		},
	}
	body, path, err := imageWireBody(input)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/images/edits" {
		t.Fatalf("path = %q", path)
	}
	var wire struct {
		Image struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"image"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Image.Type != "image_url" ||
		wire.Image.URL != input.ReferenceImages[0].Image.Source.DataURL() {
		t.Fatalf("edit image = %+v", wire.Image)
	}
}

func TestImageEditRejectsMaskInsteadOfDroppingIt(t *testing.T) {
	mask := content.ImageBlock(content.Source{
		URL: "https://example.test/mask.png",
	})
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageEdit,
		Prompt:    []content.Block{content.TextBlock("Fill the masked area")},
		ReferenceImages: []content.Block{
			content.ImageBlock(content.Source{
				URL: "https://example.test/photo.png",
			}),
		},
		Mask: &mask,
	}
	if _, _, err := imageWireBody(input); err == nil {
		t.Fatal("mask was silently dropped")
	}
}

func TestImageEditRejectsNonHTTPReferenceURL(t *testing.T) {
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageEdit,
		Prompt:    []content.Block{content.TextBlock("Make it blue")},
		ReferenceImages: []content.Block{
			content.ImageBlock(content.Source{
				URL: "https://user:secret@example.test/photo.png",
			}),
		},
	}
	if _, _, err := imageWireBody(input); err == nil {
		t.Fatal("URL with user info was sent to provider")
	}
	input.ReferenceImages[0].Image.Source.URL = "file:///tmp/photo.png"
	if _, _, err := imageWireBody(input); err == nil {
		t.Fatal("file URL was sent to provider")
	}
}

func TestVideoWireConvertsInitialFrameAndWholeSeconds(t *testing.T) {
	input := media.VideoInput{
		Model:  "grok-imagine-video-1.5",
		Prompt: []content.Block{content.TextBlock("Move slowly")},
		ReferenceFrames: []content.Block{
			content.ImageBlock(content.Source{
				URL: "https://example.test/frame.png",
			}),
		},
		Settings: media.VideoSettings{
			Duration:    5 * time.Second,
			AspectRatio: "16:9",
		},
	}
	body, err := videoWireBody(input)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Image    map[string]string `json:"image"`
		Duration int               `json:"duration"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Image["url"] != "https://example.test/frame.png" ||
		wire.Image["type"] != "" ||
		wire.Duration != 5 {
		t.Fatalf("video request = %+v", wire)
	}
	input.Settings.Duration = 1500 * time.Millisecond
	if _, err := videoWireBody(input); err == nil {
		t.Fatal("fractional seconds would be silently rounded")
	}
}

func TestDecodeImageResponseRetainsTemporaryURL(t *testing.T) {
	result, err := decodeImageWire(
		[]byte(`{"data":[{"url":"https://imgen.x.ai/tmp.jpeg","mime_type":"image/jpeg","revised_prompt":"changed text"}]}`),
		"test-grok",
		"grok-imagine-image-2.0",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 ||
		result.Content[0].Image.Source.URL != "https://imgen.x.ai/tmp.jpeg" ||
		result.Content[0].Image.Source.Data != nil {
		t.Fatalf("image content = %+v", result.Content[0])
	}
}

func TestDecodeVideoStatusRequiresOutputForDone(t *testing.T) {
	if _, err := decodeVideoWire([]byte(`{"status":"done","video":{}}`)); err == nil {
		t.Fatal("done status without video URL was accepted")
	}
	if _, err := decodeVideoWire([]byte(`{"status":"pending"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeVideoStatusRejectsInvalidReportedDuration(t *testing.T) {
	for _, seconds := range []string{"-1", "1e12"} {
		t.Run(seconds, func(t *testing.T) {
			raw := []byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/output.mp4","duration":` + seconds + `}}`)
			if _, err := decodeVideoWire(raw); err == nil {
				t.Fatal("invalid reported duration was accepted")
			}
		})
	}
}
