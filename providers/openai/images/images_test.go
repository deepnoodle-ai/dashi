package images

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

var tinyPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0, 0, 0, 0, 'I', 'H', 'D', 'R',
}

func testClient(t *testing.T, endpoint string,
	capabilities media.ImageCapabilities) *Client {
	t.Helper()
	name := "gpt-image-1.5"
	client, err := New(Config{
		Provider: "openai-test",
		APIKey:   "test-key",
		Endpoint: endpoint,
		Models: map[string]media.ModelInfo{
			name: {
				Image: &capabilities,
				Evidence: model.ModelEvidence{
					Model: name, API: API, Endpoint: endpoint,
					Owner: "test", Source: "fixture",
					Method:      model.EvidenceFixture,
					QualifiedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("construct image client: %v", err)
	}
	return client
}

func imageInput(operation media.ImageOperation) media.ImageInput {
	return media.ImageInput{
		Model:     "gpt-image-1.5",
		Operation: operation,
		Prompt:    []content.Block{content.TextBlock("A ceramic fox")},
	}
}

func writeImageResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req_123")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"created":       1_800_000_000,
		"output_format": "png",
		"usage": map[string]any{
			"total_tokens": 17,
		},
		"data": []map[string]string{{
			"b64_json": base64.StdEncoding.EncodeToString(tinyPNG),
		}},
	}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestClientGenerateImageSendsPreparedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("request lost its configured authentication")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode JSON body: %v", err)
		}
		if body["model"] != "gpt-image-1.5" ||
			body["prompt"] != "A ceramic fox" || body["n"] != float64(1) {
			t.Errorf("prepared body changed during send: %#v", body)
		}
		writeImageResponse(t, w)
	}))
	defer server.Close()
	client := testClient(t, server.URL+"/v1", media.ImageCapabilities{
		Generate: model.SupportSupported,
	})
	input := imageInput(media.ImageGenerate)
	request, err := client.PrepareImage(context.Background(), input)
	if err != nil {
		t.Fatalf("prepare image: %v", err)
	}
	input.Prompt[0].Text.Text = "changed after preparation"
	if request.Snapshot().Effective.Count != 1 {
		t.Fatal("provider default count was not recorded")
	}
	returnedBody := request.Body()
	returnedBody[0] = 'X'
	result, err := client.SendImage(context.Background(), request)
	if err != nil {
		t.Fatalf("send image: %v", err)
	}
	if result.ResponseID != "req_123" || result.Requested.Model != "gpt-image-1.5" {
		t.Fatalf("result lost request provenance: %#v", result)
	}
	if len(result.Content) != 1 ||
		!bytes.Equal(result.Content[0].Image.Source.Data, tinyPNG) {
		t.Fatal("result did not retain image bytes")
	}
	if result.Content[0].Image.Source.MIMEType != "image/png" {
		t.Fatal("result did not retain the response image format")
	}
	if string(result.RawUsage) != `{"total_tokens":17}` {
		t.Fatalf("provider usage was not retained: %s", result.RawUsage)
	}
}

func TestClientEditImageUploadsMultipartBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || params["boundary"] == "" {
			t.Errorf("missing multipart boundary: %v", err)
			return
		}
		parts := multipart.NewReader(r.Body, params["boundary"])
		var files int
		for {
			part, err := parts.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read multipart part: %v", err)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("read multipart bytes: %v", err)
				return
			}
			if part.FormName() == "image" || part.FormName() == "mask" {
				files++
				if !bytes.Equal(data, tinyPNG) {
					t.Error("multipart upload changed inline image bytes")
				}
			}
		}
		if files != 2 {
			t.Errorf("expected image and mask uploads, got %d", files)
		}
		writeImageResponse(t, w)
	}))
	defer server.Close()
	client := testClient(t, server.URL+"/v1", media.ImageCapabilities{
		Edit: model.SupportSupported,
		Mask: model.SupportSupported,
	})
	input := imageInput(media.ImageEdit)
	image := content.ImageBlock(content.Source{
		Data: slices.Clone(tinyPNG), MIMEType: "image/png",
	})
	input.ReferenceImages = []content.Block{image}
	input.Mask = &image
	request, err := client.PrepareImage(context.Background(), input)
	if err != nil {
		t.Fatalf("prepare edit: %v", err)
	}
	input.ReferenceImages[0].Image.Source.Data[0] = 0
	if !strings.HasPrefix(request.ContentType(), "multipart/form-data;") {
		t.Fatal("inline edit did not prepare multipart")
	}
	returnedBody := request.Body()
	returnedBody[0] = 'X'
	if _, err := client.SendImage(context.Background(), request); err != nil {
		t.Fatalf("send edit: %v", err)
	}
}

func TestClientUnknownCapabilityIsExplicit(t *testing.T) {
	strict, err := New(Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.PrepareImage(context.Background(),
		imageInput(media.ImageGenerate)); err == nil {
		t.Fatal("missing model evidence was silently accepted")
	}
	permissive, err := New(Config{
		APIKey: "test", UnknownCapabilities: model.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := permissive.PrepareImage(context.Background(),
		imageInput(media.ImageGenerate))
	if err != nil {
		t.Fatalf("explicit unknown allowance failed: %v", err)
	}
	if !slices.Contains(request.Snapshot().Unverified, "image.generate") {
		t.Fatal("unverified generation was not recorded")
	}
}

func TestClientAllowsFutureModelWithExplicitUncertainty(t *testing.T) {
	client, err := New(Config{
		APIKey: "test", UnknownCapabilities: model.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := imageInput(media.ImageGenerate)
	input.Model = "future-image-model"
	input.Settings.Quality = "max"
	input.Settings.Size = media.Size{Width: 4096, Height: 4096}
	request, err := client.PrepareImage(context.Background(), input)
	if err != nil {
		t.Fatalf("future model was blocked despite explicit allowance: %v", err)
	}
	if !slices.Contains(request.Snapshot().Unverified, "image.generate") ||
		!slices.Contains(request.Snapshot().Unverified, "image.quality") ||
		!slices.Contains(request.Snapshot().Unverified, "image.size") {
		t.Fatalf("missing uncertainty record: %#v", request.Snapshot().Unverified)
	}
}

func TestClientRejectsKnownUnsupportedCapability(t *testing.T) {
	client := testClient(t, defaultEndpoint, media.ImageCapabilities{
		Generate: model.SupportUnsupported,
	})
	_, err := client.PrepareImage(context.Background(),
		imageInput(media.ImageGenerate))
	var mediaError *media.Error
	if !errors.As(err, &mediaError) ||
		mediaError.Category != media.ErrorUnsupported {
		t.Fatalf("known unsupported generation was not refused: %v", err)
	}
}

func TestClientEditImagePreservesURLReference(t *testing.T) {
	client, err := New(Config{
		APIKey: "test", UnknownCapabilities: model.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := imageInput(media.ImageEdit)
	input.ReferenceImages = []content.Block{content.ImageBlock(
		content.Source{URL: "https://example.com/reference.png"},
	)}
	request, err := client.PrepareImage(context.Background(), input)
	if err != nil {
		t.Fatalf("prepare URL edit: %v", err)
	}
	if request.ContentType() != "application/json" {
		t.Fatal("URL reference should be encoded as a JSON reference")
	}
	var body struct {
		Images []struct {
			ImageURL string `json:"image_url"`
		} `json:"images"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Images) != 1 ||
		body.Images[0].ImageURL != "https://example.com/reference.png" {
		t.Fatalf("URL reference was dropped: %#v", body.Images)
	}
}

func TestClientRejectsWrongDestinationBeforeDispatch(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		calls++
		writeImageResponse(t, w)
	}))
	defer server.Close()
	endpoint := server.URL + "/v1"
	first := testClient(t, endpoint, media.ImageCapabilities{
		Generate: model.SupportSupported,
	})
	second, err := New(Config{
		Provider: "openai-test", APIKey: "another-key", Endpoint: endpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := first.PrepareImage(context.Background(),
		imageInput(media.ImageGenerate))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendImage(context.Background(), request); err == nil {
		t.Fatal("wrong credential scope was accepted")
	}
	if calls != 0 {
		t.Fatal("mismatched request reached the provider")
	}
}

func TestClientClassifiesQuotaRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_quota"}}`))
	}))
	defer server.Close()
	client := testClient(t, server.URL+"/v1", media.ImageCapabilities{
		Generate: model.SupportSupported,
	})
	request, err := client.PrepareImage(context.Background(),
		imageInput(media.ImageGenerate))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SendImage(context.Background(), request)
	var providerError *media.Error
	if !errors.As(err, &providerError) {
		t.Fatalf("expected media.Error, got %v", err)
	}
	if providerError.Category != media.ErrorRateLimited ||
		providerError.Code != "insufficient_quota" {
		t.Fatalf("quota rejection classification lost: %#v", providerError)
	}
	if providerError.Retryable {
		t.Fatal("insufficient quota was advised as retryable")
	}
	if providerError.RetryAfter != 12*time.Second {
		t.Fatal("retry-after hint was lost")
	}
}

func TestClientRejectsImageSettingsItCannotEncode(t *testing.T) {
	client, err := New(Config{
		APIKey: "test", UnknownCapabilities: model.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		input media.ImageInput
	}{
		{
			name: "generation with reference",
			input: func() media.ImageInput {
				input := imageInput(media.ImageGenerate)
				input.ReferenceImages = []content.Block{content.ImageBlock(
					content.Source{Data: tinyPNG, MIMEType: "image/png"},
				)}
				return input
			}(),
		},
		{
			name: "unsupported aspect ratio",
			input: func() media.ImageInput {
				input := imageInput(media.ImageGenerate)
				input.Settings.AspectRatio = "16:9"
				return input
			}(),
		},
		{
			name: "edit with unqualified file ID",
			input: func() media.ImageInput {
				input := imageInput(media.ImageEdit)
				input.ReferenceImages = []content.Block{content.ImageBlock(
					content.Source{
						ID: "file_123",
						Origin: content.Origin{
							Provider: "openai", API: "openai.files",
							Scope: "different-credential",
						},
					},
				)}
				return input
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.PrepareImage(context.Background(), tc.input); err == nil {
				t.Fatal("unsupported input was silently encoded")
			}
		})
	}
}

func TestDecodeImageSourceMarksDalleURLExpiry(t *testing.T) {
	const created = int64(1_800_000_000)
	source, err := decodeImageSource(
		"", "https://example.com/generated.png", "", created, "dall-e-3",
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.URL != "https://example.com/generated.png" {
		t.Fatal("provider URL was lost")
	}
	if source.ExpiresAt == nil ||
		!source.ExpiresAt.Equal(time.Unix(created, 0).Add(time.Hour)) {
		t.Fatal("DALL-E URL expiry was not preserved")
	}
}

func TestDecodeImageSourceRejectsFalseFormat(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(tinyPNG)
	if _, err := decodeImageSource(encoded, "", "jpeg", 0,
		"gpt-image-1.5"); err == nil {
		t.Fatal("declared JPEG with PNG bytes was accepted")
	}
}
