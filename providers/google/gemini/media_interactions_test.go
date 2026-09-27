package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

func newInteractionsFixture(t *testing.T, server *httptest.Server) *InteractionsMediaClient {
	t.Helper()
	p, err := NewInteractionsMedia(InteractionsMediaConfig{Provider: "google-test", APIKey: "fixture-key",
		Endpoint: server.URL + "/v1beta/interactions", UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInteractionsImageEditAndInterleavedOutput(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/interactions" ||
			r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
			t.Errorf("unexpected Interactions request: %s %s", r.Method, r.URL)
		}
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				MIMEType string `json:"mime_type"`
				Data     string `json:"data"`
			} `json:"input"`
			ResponseFormat struct {
				Type        string `json:"type"`
				Delivery    string `json:"delivery"`
				AspectRatio string `json:"aspect_ratio"`
				ImageSize   string `json:"image_size"`
				MIMEType    string `json:"mime_type"`
			} `json:"response_format"`
			Store      bool `json:"store"`
			Background bool `json:"background"`
			Stream     bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "gemini-3.1-flash-image" || len(body.Input) != 2 ||
			body.Input[0].Text != "change color" || body.Input[1].Type != "image" ||
			body.Input[1].Data != base64.StdEncoding.EncodeToString([]byte("input-image")) ||
			body.ResponseFormat.Type != "image" || body.ResponseFormat.Delivery != "" ||
			body.ResponseFormat.ImageSize != "2K" || body.ResponseFormat.AspectRatio != "16:9" ||
			body.ResponseFormat.MIMEType != "image/png" ||
			body.Store || body.Background || body.Stream {
			t.Errorf("unexpected image body: %+v", body)
		}
		fmt.Fprintf(w, `{"id":"i-1","status":"completed","model":"gemini-3.1-flash-image","usage":{"total_tokens":123},"steps":[{"type":"user_input","content":[{"type":"text","text":"private prompt"}]},{"type":"model_output","content":[{"type":"text","text":"first"},{"type":"image","mime_type":"image/png","data":"%s"},{"type":"text","text":"last"}]}]}`,
			base64.StdEncoding.EncodeToString([]byte("output-image")))
	}))
	defer server.Close()
	p := newInteractionsFixture(t, server)
	input := media.ImageInput{Model: "gemini-3.1-flash-image", Operation: media.ImageEdit,
		Prompt: []content.Block{content.TextBlock("change color")},
		ReferenceImages: []content.Block{content.ImageBlock(content.Source{
			Data: []byte("input-image"), MIMEType: "image/png"})},
		Settings: media.ImageSettings{Resolution: "2K", AspectRatio: "16:9", OutputMIMEType: "image/png"}}
	request, err := p.PrepareImage(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || len(request.Snapshot().Unverified) == 0 {
		t.Fatalf("preparation dispatched or lost uncertainty: sends=%d", sends)
	}
	input.ReferenceImages[0].Image.Source.Data[0] = 'X'
	result, err := p.SendImage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 1 || result.ResponseID != "i-1" ||
		!strings.Contains(string(result.RawUsage), `"total_tokens":123`) ||
		len(result.Content) != 3 || result.Content[0].Text.Text != "first" ||
		string(result.Content[1].Image.Source.Data) != "output-image" ||
		result.Content[2].Text.Text != "last" {
		t.Fatalf("unexpected image result: %+v", result)
	}
}

func TestInteractionsOmniVideoCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		format := body["response_format"].(map[string]any)
		config := body["generation_config"].(map[string]any)
		videoConfig := config["video_config"].(map[string]any)
		if body["model"] != "gemini-omni-1.1-flash" || body["store"] != false ||
			format["type"] != "video" || format["delivery"] != "inline" ||
			format["resolution"] != "720p" || format["aspect_ratio"] != "9:16" ||
			videoConfig["task"] != "image_to_video" || len(body["input"].([]any)) != 2 {
			t.Errorf("unexpected Omni body: %+v", body)
		}
		fmt.Fprintf(w, `{"id":"v-1","status":"completed","model":"gemini-omni-1.1-flash","usage":{"total_tokens":44},"steps":[{"type":"thought","content":[{"type":"text","text":"hidden"}]},{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"%s"}]}]}`,
			base64.StdEncoding.EncodeToString([]byte("mp4-data")))
	}))
	defer server.Close()
	p := newInteractionsFixture(t, server)
	request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "gemini-omni-1.1-flash",
		Prompt: []content.Block{content.TextBlock("animate this")},
		ReferenceFrames: []content.Block{content.ImageBlock(content.Source{
			Data: []byte("frame"), MIMEType: "image/png"})},
		Settings: media.VideoSettings{AspectRatio: "9:16", Resolution: "720p"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.SendVideo(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResponseID != "v-1" || len(result.Content) != 1 ||
		string(result.Content[0].Video.Source.Data) != "mp4-data" ||
		!strings.Contains(string(result.RawUsage), `"total_tokens":44`) {
		t.Fatalf("unexpected Omni result: %+v", result)
	}
}

func TestInteractionsImageModelLimitsAndRetiredImagen(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	for _, tc := range []struct{ model, resolution string }{
		{"gemini-3.1-flash-lite-image", "2K"},
		{"gemini-3-pro-image", "512"},
		{"gemini-2.5-flash-image", "4K"},
		{"imagen-4.0-generate-001", "1K"},
	} {
		t.Run(tc.model+"/"+tc.resolution, func(t *testing.T) {
			_, err := p.PrepareImage(context.Background(), media.ImageInput{
				Model: tc.model, Operation: media.ImageGenerate,
				Prompt:   []content.Block{content.TextBlock("a cat")},
				Settings: media.ImageSettings{Resolution: tc.resolution}})
			if err == nil {
				t.Fatal("expected documented model limit")
			}
		})
	}
	for _, modelName := range []string{"gemini-3.1-flash-lite-image", "gemini-3-pro-image", "gemini-2.5-flash-image"} {
		_, err := p.PrepareImage(context.Background(), media.ImageInput{Model: modelName,
			Operation: media.ImageGenerate, Prompt: []content.Block{content.TextBlock("a cat")},
			Settings: media.ImageSettings{AspectRatio: "1:8"}})
		if err == nil {
			t.Fatalf("%s accepted Flash-only aspect ratio", modelName)
		}
	}
	_, err := p.PrepareImage(context.Background(), media.ImageInput{
		Model: "gemini-3.1-flash-image", Operation: media.ImageGenerate,
		Prompt:   []content.Block{content.TextBlock("a cat")},
		Settings: media.ImageSettings{Resolution: "512", AspectRatio: "1:8"}})
	if err != nil {
		t.Fatalf("Flash Image 512 and 1:8 should prepare: %v", err)
	}
}

func TestInteractionsOmniRejectsUnsupportedShape(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	for _, tc := range []struct {
		name  string
		input media.VideoInput
	}{
		{"high resolution", media.VideoInput{Model: "gemini-omni-1.1-flash",
			Prompt:   []content.Block{content.TextBlock("a scene")},
			Settings: media.VideoSettings{Resolution: "1080p"}}},
		{"duration", media.VideoInput{Model: "gemini-omni-1.1-flash",
			Prompt:   []content.Block{content.TextBlock("a scene")},
			Settings: media.VideoSettings{Duration: 6 * time.Second}}},
		{"edit without video source", media.VideoInput{Model: "gemini-omni-1.1-flash",
			Operation: media.VideoEdit, Prompt: []content.Block{content.TextBlock("change color")}}},
		{"ambiguous frames", media.VideoInput{Model: "gemini-omni-1.1-flash",
			ReferenceFrames: []content.Block{
				content.ImageBlock(content.Source{Data: []byte("first"), MIMEType: "image/png"}),
				content.ImageBlock(content.Source{Data: []byte("last"), MIMEType: "image/png"}),
			}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.PrepareVideo(context.Background(), tc.input); err == nil {
				t.Fatal("expected unsupported shape")
			}
		})
	}
}

func TestInteractionsRejectsURIAndIncompleteCall(t *testing.T) {
	for _, payload := range []string{
		`{"id":"v-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://evil.example/video"}]}]}`,
		`{"id":"v-3","status":"in_progress","steps":[]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, payload)
		}))
		p := newInteractionsFixture(t, server)
		request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "gemini-omni-1.1-flash",
			Prompt: []content.Block{content.TextBlock("a sunrise")}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.SendVideo(context.Background(), request)
		if err == nil || result.ResponseID == "" {
			t.Fatalf("expected identified protocol error: result=%+v, err=%v", result, err)
		}
		server.Close()
	}
}

func TestInteractionsPreparedRequestIsCredentialScoped(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends++
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	first := newInteractionsFixture(t, server)
	request, err := first.PrepareImage(context.Background(), media.ImageInput{
		Model: "gemini-3.1-flash-image", Operation: media.ImageGenerate,
		Prompt: []content.Block{content.TextBlock("a cat")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewInteractionsMedia(InteractionsMediaConfig{Provider: "google-test",
		APIKey: "other-key", Endpoint: server.URL + "/v1beta/interactions",
		UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendImage(context.Background(), request); err == nil || sends != 0 {
		t.Fatalf("cross-credential request reached network: sends=%d, err=%v", sends, err)
	}
}

func TestVeoRetiredAndLiteModelLimits(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newMediaFixture(t, server)
	for _, tc := range []struct {
		model    string
		settings media.VideoSettings
	}{
		{"veo-3.1-lite-generate-preview", media.VideoSettings{Duration: 8 * time.Second, Resolution: "4k"}},
		{"veo-3.0-generate-001", media.VideoSettings{}},
		{"veo-3.0-fast-generate-001", media.VideoSettings{}},
		{"veo-2.0-generate-001", media.VideoSettings{}},
	} {
		_, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: tc.model,
			Prompt: []content.Block{content.TextBlock("a sunrise")}, Settings: tc.settings})
		if err == nil {
			t.Fatalf("expected model limit for %s %+v", tc.model, tc.settings)
		}
	}
}
