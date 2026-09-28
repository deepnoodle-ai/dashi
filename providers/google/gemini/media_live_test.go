//go:build live

package gemini

import (
	"bytes"
	"context"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

// Run with KERNEL_LIVE_MEDIA_TEST=1, GEMINI_API_KEY, and GEMINI_IMAGE_MODEL.
// Each subtest makes one paid image call. KERNEL_LIVE_IMAGE_DIR
// optionally saves the generated image for visual inspection.
func TestLiveImageGeneration(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_MEDIA_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_MEDIA_TEST=1 to make paid image calls")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_IMAGE_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_IMAGE_MODEL are required")
	}
	input := media.ImageInput{
		Model:     modelName,
		Operation: media.ImageGenerate,
		Prompt: []content.Block{
			content.TextBlock("A simple blue ceramic cup on a plain warm white background, centered, no text."),
		},
		Settings: media.ImageSettings{AspectRatio: "1:1", Resolution: "1K"},
	}
	t.Run("generate_content", func(t *testing.T) {
		client, err := NewMedia(MediaConfig{
			APIKey:           key,
			CapabilityPolicy: model.CapabilityAdjust,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		request, err := client.PrepareImage(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendImage(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		checkLiveImage(t, result, ImageAPI, modelName, "generate-content")
	})
	t.Run("interactions", func(t *testing.T) {
		client, err := NewInteractionsMedia(InteractionsMediaConfig{
			APIKey:           key,
			CapabilityPolicy: model.CapabilityAdjust,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		request, err := client.PrepareImage(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendImage(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		checkLiveImage(t, result, InteractionsAPI, modelName, "interactions")
	})
}

func TestLiveImageEdit(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_MEDIA_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_MEDIA_TEST=1 to make paid image calls")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_IMAGE_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_IMAGE_MODEL are required")
	}
	reference := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			reference.Set(x, y, color.RGBA{B: 200, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, reference); err != nil {
		t.Fatal(err)
	}
	input := media.ImageInput{
		Model:     modelName,
		Operation: media.ImageEdit,
		Prompt: []content.Block{
			content.TextBlock("Edit this image by adding a small red circle in the center. Keep the blue background. No text."),
		},
		ReferenceImages: []content.Block{
			content.ImageBlock(content.Source{
				Data:     encoded.Bytes(),
				MIMEType: "image/png",
			}),
		},
		Settings: media.ImageSettings{AspectRatio: "1:1", Resolution: "1K"},
	}
	t.Run("generate_content", func(t *testing.T) {
		client, err := NewMedia(MediaConfig{
			APIKey:           key,
			CapabilityPolicy: model.CapabilityAdjust,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		request, err := client.PrepareImage(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendImage(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		checkLiveImage(t, result, ImageAPI, modelName, "generate-content-edit")
	})
	t.Run("interactions", func(t *testing.T) {
		client, err := NewInteractionsMedia(InteractionsMediaConfig{
			APIKey:           key,
			CapabilityPolicy: model.CapabilityAdjust,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		request, err := client.PrepareImage(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendImage(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		checkLiveImage(t, result, InteractionsAPI, modelName, "interactions-edit")
	})
}

func checkLiveImage(
	t *testing.T,
	result media.Result,
	api, modelName, name string,
) {
	t.Helper()
	if err := result.Validate(); err != nil ||
		result.API != api ||
		result.Requested.Model != modelName {
		t.Fatalf(
			"incomplete image result: validation=%v api=%q requested=%q",
			err,
			result.API,
			result.Requested.Model,
		)
	}
	for _, block := range result.Content {
		if block.Kind != content.KindImage {
			continue
		}
		source := block.Image.Source
		config, format, err := image.DecodeConfig(bytes.NewReader(source.Data))
		if err != nil || config.Width < 256 || config.Height < 256 ||
			!strings.EqualFold(source.MIMEType, "image/"+format) {
			t.Fatalf(
				"invalid generated image: mime=%q format=%q size=%dx%d decode=%v",
				source.MIMEType,
				format,
				config.Width,
				config.Height,
				err,
			)
		}
		if dir := os.Getenv("KERNEL_LIVE_IMAGE_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name+"."+format)
			if err := os.WriteFile(path, source.Data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("saved image: %s", path)
		}
		t.Logf(
			"generated %s image: %dx%d, %d bytes",
			format,
			config.Width,
			config.Height,
			len(source.Data),
		)
		return
	}
	t.Fatal("image result has no image block")
}
