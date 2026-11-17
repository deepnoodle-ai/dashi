//go:build live

package images

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

// Run with DASHI_LIVE_MEDIA_TEST=1, OPENAI_API_KEY, and OPENAI_IMAGE_MODEL.
// The test makes one paid low-quality call. OPENAI_IMAGE_FORMAT
// (png, jpeg, webp, or gif) and OPENAI_IMAGE_COMPLEXITY are optional.
func TestLiveImageGeneration(t *testing.T) {
	if os.Getenv("DASHI_LIVE_MEDIA_TEST") != "1" {
		t.Skip("set DASHI_LIVE_MEDIA_TEST=1 to make paid image calls")
	}
	modelName := os.Getenv("OPENAI_IMAGE_MODEL")
	if os.Getenv("OPENAI_API_KEY") == "" || modelName == "" {
		t.Fatal("OPENAI_API_KEY and OPENAI_IMAGE_MODEL are required")
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
			Size:    liveImageSize(),
			Quality: "low",
		},
	}
	if format := os.Getenv("OPENAI_IMAGE_FORMAT"); format != "" {
		input.Settings.OutputMIMEType = "image/" + format
	}
	options := Options{Complexity: os.Getenv("OPENAI_IMAGE_COMPLEXITY")}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	request, err := client.PrepareImageWithOptions(ctx, input, options)
	if err != nil {
		t.Fatal(err)
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
	t.Logf("%s returned %s, %d bytes, usage %s",
		modelName, source.MIMEType, len(source.Data), usage.String())
}

// liveImageSize is 1024x1024, or 816x816 for GIF output: a GIF's frames are
// drawn on one sheet, and nine 1024x1024 frames exceed the pixel budget.
func liveImageSize() media.Size {
	if os.Getenv("OPENAI_IMAGE_FORMAT") == "gif" {
		return media.Size{Width: 816, Height: 816}
	}
	return media.Size{Width: 1024, Height: 1024}
}
