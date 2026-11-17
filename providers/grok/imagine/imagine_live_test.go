//go:build live

package imagine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

// Run with DASHI_LIVE_MEDIA_TEST=1, XAI_API_KEY, and XAI_IMAGE_MODEL. The
// test makes one paid image call. XAI_IMAGE_ASPECT_RATIO, XAI_IMAGE_QUALITY,
// and XAI_IMAGE_RESOLUTION are optional.
func TestLiveImageGeneration(t *testing.T) {
	if os.Getenv("DASHI_LIVE_MEDIA_TEST") != "1" {
		t.Skip("set DASHI_LIVE_MEDIA_TEST=1 to make paid image calls")
	}
	modelName := os.Getenv("XAI_IMAGE_MODEL")
	if os.Getenv("XAI_API_KEY") == "" || modelName == "" {
		t.Fatal("XAI_API_KEY and XAI_IMAGE_MODEL are required")
	}
	client, err := New(Config{CapabilityPolicy: model.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	input := media.ImageInput{
		Model:     modelName,
		Operation: media.ImageGenerate,
		Prompt: []content.Block{
			content.TextBlock("A simple blue ceramic cup on a plain white background, no text."),
		},
		Settings: media.ImageSettings{
			AspectRatio: os.Getenv("XAI_IMAGE_ASPECT_RATIO"),
			Quality:     os.Getenv("XAI_IMAGE_QUALITY"),
			Resolution:  os.Getenv("XAI_IMAGE_RESOLUTION"),
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	request, err := client.PrepareImageWithOptions(
		ctx,
		input,
		ImageOptions{ResponseFormat: ImageResponseBase64},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, adjustment := range request.Snapshot().Adjustments {
		t.Logf("adjustment: %+v", adjustment)
	}
	result, err := client.SendImage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Image == nil ||
		len(result.Content[0].Image.Source.Data) == 0 {
		t.Fatalf("no image returned: %+v", result)
	}
	source := result.Content[0].Image.Source
	var usage bytes.Buffer
	_ = json.Compact(&usage, result.RawUsage)
	t.Logf("%s returned %s, %d bytes, served %q, usage %s",
		modelName, source.MIMEType, len(source.Data), result.Served.Model,
		usage.String())
}
