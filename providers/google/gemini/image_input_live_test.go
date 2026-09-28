//go:build live

package gemini

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"google.golang.org/genai"
)

// Run with DASHI_LIVE_IMAGE_INPUT_TEST=1, GEMINI_API_KEY, and GEMINI_MODEL.
// The inline blocking, inline streaming, URL, and file URI subtests each make
// one paid Gemini LLM call. The file URI subtest uploads and deletes a test
// PNG.
func TestLiveImageInput(t *testing.T) {
	if os.Getenv("DASHI_LIVE_IMAGE_INPUT_TEST") != "1" {
		t.Skip("set DASHI_LIVE_IMAGE_INPUT_TEST=1 to make paid Gemini LLM calls")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_MODEL are required")
	}
	client, err := New(Config{
		APIKey:           key,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	red := liveCircleImage(t, color.RGBA{R: 255, A: 255})
	green := liveCircleImage(t, color.RGBA{G: 255, A: 255})
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"blocking", false}, {"streaming", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			input := llm.Input{
				Model:    modelName,
				Stream:   tc.stream,
				Settings: llm.Settings{MaxTokens: 512},
				Messages: []llm.Message{
					llm.UserMessage(
						llm.Text("What color is the circle in each image, in order? Reply with the two colors."),
						llm.Block(content.ImageBlock(content.Source{
							Data:     red,
							MIMEType: "image/png",
						})),
						llm.Block(content.ImageBlock(content.Source{
							Data:     green,
							MIMEType: "image/png",
						})),
					),
				},
			}
			request, err := client.Prepare(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Send(ctx, llm.Call{Request: request})
			if err != nil {
				t.Fatal(err)
			}
			answer := strings.ToLower(result.Message.Text())
			redAt, greenAt := strings.Index(
				answer,
				"red",
			), strings.Index(
				answer,
				"green",
			)
			if result.Stop.Kind != llm.StopFinished || redAt < 0 || greenAt <= redAt {
				t.Fatalf("image input answer = %q, stop = %q", answer, result.Stop.Kind)
			}
			t.Logf("model=%s stream=%t answer=%q", modelName, tc.stream, answer)
		})
	}
	t.Run("remote_url", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		input := llm.Input{
			Model:    modelName,
			Settings: llm.Settings{MaxTokens: 512},
			Messages: []llm.Message{
				llm.UserMessage(
					llm.Text("What musical instrument is pictured? Reply with its name."),
					llm.Block(content.ImageBlock(content.Source{
						URL:      "https://goo.gle/instrument-img",
						MIMEType: "image/jpeg",
					})),
				),
			},
		}
		request, err := client.Prepare(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Send(ctx, llm.Call{Request: request})
		if err != nil {
			t.Fatal(err)
		}
		answer := strings.ToLower(result.Message.Text())
		if result.Stop.Kind != llm.StopFinished || !strings.Contains(
			answer,
			"organ",
		) {
			t.Fatalf("remote image answer = %q, stop = %q", answer, result.Stop.Kind)
		}
		t.Logf("model=%s remote image answer=%q", modelName, answer)
	})
	t.Run("file_uri", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		files, err := genai.NewClient(
			ctx,
			&genai.ClientConfig{
				APIKey:  key,
				Backend: genai.BackendGeminiAPI,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		file, err := files.Files.Upload(
			ctx,
			bytes.NewReader(red),
			&genai.UploadFileConfig{
				MIMEType: "image/png",
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(
				context.Background(),
				15*time.Second,
			)
			defer cleanupCancel()
			if _, err := files.Files.Delete(cleanupCtx, file.Name, nil); err != nil {
				t.Errorf("delete uploaded test image: %v", err)
			}
		})
		httpClient, err := client.httpClient(modelName)
		if err != nil {
			t.Fatal(err)
		}
		input := llm.Input{
			Model:    modelName,
			Settings: llm.Settings{MaxTokens: 512},
			Messages: []llm.Message{
				llm.UserMessage(
					llm.Text("What color is the circle in this image? Reply with the color."),
					llm.Block(content.ImageBlock(content.Source{
						URI:      file.URI,
						MIMEType: file.MIMEType,
						Origin:   httpClient.Origin(modelName),
					})),
				),
			},
		}
		request, err := client.Prepare(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.Send(ctx, llm.Call{Request: request})
		if err != nil {
			t.Fatal(err)
		}
		answer := strings.ToLower(result.Message.Text())
		if result.Stop.Kind != llm.StopFinished || !strings.Contains(
			answer,
			"red",
		) {
			t.Fatalf("file image answer = %q, stop = %q", answer, result.Stop.Kind)
		}
		t.Logf("model=%s file image answer=%q", modelName, answer)
	})
}

func liveCircleImage(t *testing.T, center color.RGBA) []byte {
	t.Helper()
	const size = 256
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := color.RGBA{B: 220, A: 255}
			if dx, dy := x-size/2, y-size/2; dx*dx+dy*dy < 70*70 {
				c = center
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
