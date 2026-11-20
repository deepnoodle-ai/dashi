//go:build live

package images

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// Run with DASHI_LIVE_MEDIA_TEST=1 and MODEL_API_KEY or META_API_KEY. The
// test makes two paid calls, a generation and an edit of its output, at
// $0.01 per image.
func TestLiveImageGeneration(t *testing.T) {
	if os.Getenv("DASHI_LIVE_MEDIA_TEST") != "1" {
		t.Skip("set DASHI_LIVE_MEDIA_TEST=1 to make paid image calls")
	}
	if os.Getenv("MODEL_API_KEY") == "" && os.Getenv("META_API_KEY") == "" {
		t.Fatal("MODEL_API_KEY or META_API_KEY is required")
	}
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	generated := liveImage(t, ctx, client, media.ImageInput{
		Model:     museImage,
		Operation: media.ImageGenerate,
		Prompt: []content.Block{
			content.TextBlock("A simple blue ceramic cup on a plain white background, no text."),
		},
		Settings: media.ImageSettings{AspectRatio: "16:9", OutputMIMEType: "image/png"},
	})
	liveImage(t, ctx, client, media.ImageInput{
		Model:           museImage,
		Operation:       media.ImageEdit,
		Prompt:          []content.Block{content.TextBlock("Make the cup red.")},
		ReferenceImages: []content.Block{content.ImageBlock(generated)},
		Settings:        media.ImageSettings{OutputMIMEType: "image/jpeg"},
	})
}

func liveImage(t *testing.T, ctx context.Context, client *Client,
	input media.ImageInput) content.Source {
	t.Helper()
	options := Options{ReasoningStrength: "low"}
	request, err := client.PrepareImageWithOptions(ctx, input, options)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
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
	config, _, err := image.DecodeConfig(bytes.NewReader(source.Data))
	if err != nil {
		t.Fatalf("decode %s: %v", source.MIMEType, err)
	}
	t.Logf("%s %s returned %s %dx%d, %d bytes, in %s, usage %s",
		input.Model, input.Operation, source.MIMEType, config.Width,
		config.Height, len(source.Data), time.Since(start).Round(time.Second),
		usage.String())
	return source
}
