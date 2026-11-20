package images

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

var tinyPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}

func generateInput() media.ImageInput {
	return media.ImageInput{
		Model:     museImage,
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("A ceramic fox")},
	}
}

func defaultClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func bodyFields(t *testing.T, request media.ImageRequest) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(request.Body(), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestNewBuiltInEvidenceOnlyAtPublicEndpoint(t *testing.T) {
	client := defaultClient(t)
	info, ok := client.ModelInfo(museImage)
	if !ok || info.Image.Mask != model.SupportUnsupported ||
		info.Evidence.Endpoint != DefaultEndpoint || info.Image.MaxCount != 10 {
		t.Fatalf("built-in evidence = %+v, %v", info, ok)
	}
	if client.Provider() != "meta" || client.API() != API {
		t.Fatalf("identity = %s %s", client.Provider(), client.API())
	}
	gateway, err := New(Config{Provider: "gateway", Endpoint: "https://gateway.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.ModelInfo(museImage); ok {
		t.Fatal("custom endpoint must not inherit public evidence")
	}
	if _, err := New(Config{Endpoint: "https://gateway.example/v1"}); err == nil {
		t.Fatal("custom endpoint without provider identity was accepted")
	}
}

func TestNewReadsMetaKeysOnlyForMeta(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("META_API_KEY", "meta-secret")
	client, err := New(Config{})
	if err != nil || client.apiKey != "meta-secret" {
		t.Fatalf("meta key = %q, %v", client.apiKey, err)
	}
	t.Setenv("MODEL_API_KEY", "model-secret")
	client, _ = New(Config{})
	if client.apiKey != "model-secret" {
		t.Fatalf("MODEL_API_KEY should win, got %q", client.apiKey)
	}
	gateway, _ := New(Config{Provider: "gateway", Endpoint: "https://gateway.example/v1"})
	if gateway.apiKey != "" {
		t.Fatal("another destination read the Meta key")
	}
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("META_API_KEY", "")
	client, _ = New(Config{})
	request, err := client.PrepareImage(t.Context(), generateInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendImage(t.Context(), request); !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("SendImage without key = %v", err)
	}
}

func TestPrepareGenerateBody(t *testing.T) {
	client := defaultClient(t)
	input := generateInput()
	input.Settings = media.ImageSettings{AspectRatio: "16:9", OutputMIMEType: "image/png"}
	temperature, off := 0.5, false
	request, err := client.PrepareImageWithOptions(t.Context(), input, Options{
		ReasoningStrength: "low",
		Temperature:       &temperature,
		ToolEnablement:    &ToolEnablement{WebSearch: &off},
		User:              "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := bodyFields(t, request)
	want := map[string]any{
		"model":              museImage,
		"prompt":             "A ceramic fox",
		"size":               "1824x1026",
		"n":                  float64(1),
		"output_format":      "png",
		"reasoning_strength": "low",
		"temperature":        0.5,
		"tool_enablement":    map[string]any{"enable_web_search": false},
		"user":               "user-1",
	}
	for key, value := range want {
		got, _ := json.Marshal(fields[key])
		expected, _ := json.Marshal(value)
		if string(got) != string(expected) {
			t.Errorf("%s = %s, want %s", key, got, expected)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("fields = %v", fields)
	}
	if _, ok := fields["moderation"]; ok {
		t.Error("moderation must not be sent to Muse Image")
	}
	adjustments := request.Snapshot().Adjustments
	if len(adjustments) != 2 || adjustments[1].Setting != "size" ||
		adjustments[1].Requested != "16:9" {
		t.Fatalf("adjustments = %+v", adjustments)
	}
	if request.ContentType() != "application/json" {
		t.Fatalf("content type = %s", request.ContentType())
	}
	// Aspect ratios are not claimed, so the adjust policy records them.
	if unverified := request.Snapshot().Unverified; len(unverified) != 1 ||
		unverified[0] != "image.aspect_ratio" {
		t.Fatalf("unverified = %v", unverified)
	}
}

func TestPrepareExactSizeSendsAspectRatio(t *testing.T) {
	client := defaultClient(t)
	input := generateInput()
	input.Settings.Size = media.Size{Width: 1792, Height: 1024}
	request, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if size := bodyFields(t, request)["size"]; size != "1792x1024" {
		t.Fatalf("size = %v", size)
	}
	input.Settings.Size = media.Size{Width: 1700, Height: 100}
	if _, err := client.PrepareImage(t.Context(), input); err == nil {
		t.Fatal("17:1 size was accepted")
	}
}

func TestPrepareRejectsUnsupportedSettings(t *testing.T) {
	client := defaultClient(t)
	for name, mutate := range map[string]func(*media.ImageInput){
		"quality":      func(in *media.ImageInput) { in.Settings.Quality = "high" },
		"resolution":   func(in *media.ImageInput) { in.Settings.Resolution = "1K" },
		"count":        func(in *media.ImageInput) { in.Settings.Count = 11 },
		"ratio":        func(in *media.ImageInput) { in.Settings.AspectRatio = "17:1" },
		"ratio format": func(in *media.ImageInput) { in.Settings.AspectRatio = "19.5:9" },
		"gif":          func(in *media.ImageInput) { in.Settings.OutputMIMEType = "image/gif" },
		"mask": func(in *media.ImageInput) {
			in.Operation = media.ImageEdit
			in.ReferenceImages = []content.Block{content.ImageBlock(content.Source{Data: tinyPNG})}
			mask := content.ImageBlock(content.Source{Data: tinyPNG})
			in.Mask = &mask
		},
		"generate references": func(in *media.ImageInput) {
			in.ReferenceImages = []content.Block{content.ImageBlock(content.Source{Data: tinyPNG})}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := generateInput()
			mutate(&input)
			_, err := client.PrepareImage(t.Context(), input)
			var mediaErr *media.Error
			if !errors.As(err, &mediaErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPrepareEditImages(t *testing.T) {
	client := defaultClient(t)
	input := generateInput()
	input.Operation = media.ImageEdit
	input.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{Data: tinyPNG}),
		content.ImageBlock(content.Source{URL: "https://example.com/cat.webp"}),
		content.ImageBlock(content.Source{ID: "file-1", Origin: client.FileOrigin()}),
	}
	request, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	images, _ := json.Marshal(bodyFields(t, request)["images"])
	want := `[{"image_url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString(tinyPNG) +
		`"},{"image_url":"https://example.com/cat.webp"},{"file_id":"file-1"}]`
	if string(images) != want {
		t.Fatalf("images = %s", images)
	}

	other, _ := New(Config{APIKey: "other-key"})
	input.ReferenceImages = []content.Block{
		content.ImageBlock(content.Source{ID: "file-1", Origin: other.FileOrigin()}),
	}
	if _, err := client.PrepareImage(t.Context(), input); err == nil {
		t.Fatal("file ID from another credential was accepted")
	}
	input.ReferenceImages = make([]content.Block, 11)
	for i := range input.ReferenceImages {
		input.ReferenceImages[i] = content.ImageBlock(content.Source{Data: tinyPNG})
	}
	if _, err := client.PrepareImage(t.Context(), input); err == nil {
		t.Fatal("11 edit images were accepted")
	}
}

func testServer(t *testing.T, status int, body string,
	check func(*http.Request)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{
		Provider: "meta-test",
		APIKey:   "test-key",
		Endpoint: server.URL + "/v1",
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSendImageGenerateAndEdit(t *testing.T) {
	body := `{"created":1,"output_format":"png","data":[{"b64_json":"` +
		base64.StdEncoding.EncodeToString(tinyPNG) +
		`"}],"usage":{"input_tokens":9,"output_tokens":7,"total_tokens":16}}`
	for operation, path := range map[media.ImageOperation]string{
		media.ImageGenerate: "/v1/images/generations",
		media.ImageEdit:     "/v1/images/edits",
	} {
		client := testServer(t, http.StatusOK, body, func(r *http.Request) {
			if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer test-key" ||
				r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("request %s %s", r.URL.Path, r.Header)
			}
		})
		input := generateInput()
		input.Operation = operation
		if operation == media.ImageEdit {
			input.ReferenceImages = []content.Block{content.ImageBlock(content.Source{Data: tinyPNG})}
		}
		request, err := client.PrepareImage(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.SendImage(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Image.Source.MIMEType != "image/png" ||
			result.ResponseID != "req-1" || result.API != API ||
			!strings.Contains(string(result.RawUsage), `"total_tokens":16`) {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestSendImageRejectsOtherDestination(t *testing.T) {
	client := testServer(t, http.StatusOK, "{}", func(*http.Request) {
		t.Error("request was sent")
	})
	request, err := defaultClient(t).PrepareImage(t.Context(), generateInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendImage(t.Context(), request); err == nil {
		t.Fatal("request prepared for another destination was sent")
	}
}

func TestSendImageErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		category  media.ErrorCategory
		retryable bool
	}{
		{400, media.ErrorInvalidRequest, false},
		{401, media.ErrorAuthentication, false},
		{415, media.ErrorUnsupported, false},
		{429, media.ErrorRateLimited, true},
		{500, media.ErrorOverloaded, false},
		{503, media.ErrorOverloaded, true},
	} {
		client := testServer(t, tc.status,
			`{"error":{"type":"invalid_request_error","code":"content_policy_violation","param":"prompt","message":"no"}}`,
			nil)
		request, err := client.PrepareImage(t.Context(), generateInput())
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.SendImage(t.Context(), request)
		var mediaErr *media.Error
		if !errors.As(err, &mediaErr) || mediaErr.Category != tc.category ||
			mediaErr.Retryable != tc.retryable || mediaErr.Status != tc.status {
			t.Errorf("status %d: error = %+v", tc.status, err)
		}
	}
}
