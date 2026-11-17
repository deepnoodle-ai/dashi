package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestImageErrorNamesFinishReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"finish reason and message",
			`{"candidates":[{"finishReason":"IMAGE_RECITATION","finishMessage":"Unable to show the generated image."}]}`,
			"finish reason IMAGE_RECITATION): Unable to show the generated image.",
		},
		{
			"finish reason only",
			`{"candidates":[{"finishReason":"IMAGE_SAFETY","content":{"parts":[{"text":"no"}]}}]}`,
			"finish reason IMAGE_SAFETY)",
		},
		{
			"blocked prompt",
			`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`,
			"finish reason PROHIBITED_CONTENT)",
		},
		{
			"no reason",
			`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"ok"}]}}]}`,
			"no image in Gemini response",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := newMediaFixture(t, server)
			request, err := p.PrepareImage(t.Context(), media.ImageInput{
				Model:     "gemini-image",
				Operation: media.ImageGenerate,
				Prompt:    []content.Block{content.TextBlock("a red circle")},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.SendImage(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestImagenErrorNamesReplacement(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
	}))
	defer server.Close()
	_, err := newMediaFixture(t, server).PrepareImage(t.Context(), media.ImageInput{
		Model:     "imagen-4.0-generate-001",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("a teapot")},
	})
	if err == nil || !strings.Contains(err.Error(), "shut down") ||
		!strings.Contains(err.Error(), "gemini-nano-banana-2.1") || sends != 0 {
		t.Fatalf("error = %v, sends = %d", err, sends)
	}
}
