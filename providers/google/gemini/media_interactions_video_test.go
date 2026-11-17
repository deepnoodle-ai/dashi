package gemini

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestOmniDurationIsSentAsWholeSeconds(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	input := media.VideoInput{
		Model:    "gemini-omni-1.2-flash",
		Prompt:   []content.Block{content.TextBlock("a scene")},
		Settings: media.VideoSettings{Duration: 15 * time.Second},
	}
	request, err := p.PrepareVideo(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ResponseFormat map[string]any `json:"response_format"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ResponseFormat["duration"] != "15s" {
		t.Fatalf("response_format = %v", body.ResponseFormat)
	}
	for _, d := range []time.Duration{
		2 * time.Second,
		16 * time.Second,
		3500 * time.Millisecond,
	} {
		input.Settings.Duration = d
		if _, err := p.PrepareVideo(t.Context(), input); err == nil {
			t.Errorf("duration %s was accepted", d)
		}
	}
}

func TestOmniFlashPreviewIsShutDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	if _, err := p.PrepareVideo(t.Context(), media.VideoInput{
		Model:  "gemini-omni-flash-preview",
		Prompt: []content.Block{content.TextBlock("a scene")},
	}); err == nil {
		t.Fatal("gemini-omni-flash-preview was accepted")
	}
}
