package imagine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

func TestImageBase64OptionReturnsInlineBytesWithoutResponseMIME(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(
		&encoded,
		image.NewRGBA(image.Rect(0, 0, 1, 1)),
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ResponseFormat string `json:"response_format"`
		}
		if err := json.Unmarshal(
			body,
			&request,
		); err != nil || request.ResponseFormat != "b64_json" {
			t.Errorf("unexpected image request: %s, %v", body, err)
		}
		_, _ = io.WriteString(
			w,
			`{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(encoded.Bytes())+`"}]}`,
		)
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("A green hill")},
	}
	request, err := client.PrepareImageWithOptions(
		context.Background(),
		input,
		ImageOptions{
			ResponseFormat: ImageResponseBase64,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Snapshot().ProviderOptions) == 0 {
		t.Fatal("response format was not recorded")
	}
	result, err := client.SendImage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	source := result.Content[0].Image.Source
	if source.MIMEType != "image/png" || !bytes.Equal(
		source.Data,
		encoded.Bytes(),
	) || source.URL != "" {
		t.Fatalf("unexpected inline image: %+v", source)
	}
}

func TestFileIDInputsRequireMatchingFilesOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preparation unexpectedly made a network request")
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityAdjust,
	)
	file := content.Source{
		ID:       "file_123",
		Origin:   client.FileOrigin(),
		MIMEType: "image/png",
	}
	imageInput := media.ImageInput{
		Model:           "grok-imagine-image-2.0",
		Operation:       media.ImageEdit,
		Prompt:          []content.Block{content.TextBlock("Make it blue")},
		ReferenceImages: []content.Block{content.ImageBlock(file)},
	}
	request, err := client.PrepareImage(context.Background(), imageInput)
	if err != nil {
		t.Fatal(err)
	}
	var imageWire struct {
		Image map[string]string `json:"image"`
	}
	if err := json.Unmarshal(
		request.Body(),
		&imageWire,
	); err != nil || imageWire.Image["file_id"] != "file_123" {
		t.Fatalf("file image was not encoded: %s, %v", request.Body(), err)
	}
	videoInput := media.VideoInput{
		Model:           "grok-imagine-video-1.5",
		Prompt:          []content.Block{content.TextBlock("Animate")},
		ReferenceFrames: []content.Block{content.ImageBlock(file)},
	}
	videoRequest, err := client.PrepareVideo(context.Background(), videoInput)
	if err != nil {
		t.Fatal(err)
	}
	var videoWire struct {
		Image map[string]string `json:"image"`
	}
	if err := json.Unmarshal(
		videoRequest.Body(),
		&videoWire,
	); err != nil || videoWire.Image["file_id"] != "file_123" {
		t.Fatalf("file frame was not encoded: %s, %v", videoRequest.Body(), err)
	}
	other, err := New(Config{
		Provider:         "test-grok",
		Endpoint:         server.URL + "/v1",
		APIKey:           "other-key",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	imageInput.ReferenceImages[0].Image.Source.Origin = other.FileOrigin()
	if _, err := client.PrepareImage(
		context.Background(),
		imageInput,
	); err == nil {
		t.Fatal("image file from another credential scope was accepted")
	}
	videoInput.ReferenceFrames[0].Image.Source.Origin = other.FileOrigin()
	if _, err := client.PrepareVideo(
		context.Background(),
		videoInput,
	); err == nil {
		t.Fatal("video file from another credential scope was accepted")
	}
}

func TestInvalidImageResponseFormatAndPayload(t *testing.T) {
	input := media.ImageInput{
		Model:     "grok-imagine-image-2.0",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("A hill")},
	}
	if _, _, err := imageWireBodyWithOptions(
		input,
		ImageOptions{
			ResponseFormat: "raw",
		},
		content.Origin{},
	); err == nil {
		t.Fatal("unknown image response format was accepted")
	}
	if _, err := decodeImageWire(
		[]byte(`{"data":[{"b64_json":"bm90IGFuIGltYWdl"}]}`),
		"grok",
		input.Model,
	); err == nil {
		t.Fatal("unknown inline MIME type was accepted")
	}
}

func TestInlineImageRejectsNonImageMIME(t *testing.T) {
	bad := content.ImageBlock(content.Source{
		Data:     []byte("not an image"),
		MIMEType: "application/octet-stream",
	})
	input := media.ImageInput{
		Model:           "grok-imagine-image-2.0",
		Operation:       media.ImageEdit,
		Prompt:          []content.Block{content.TextBlock("Make it blue")},
		ReferenceImages: []content.Block{bad},
	}
	if _, _, err := imageWireBody(input); err == nil {
		t.Fatal("non-image inline bytes were accepted for image edit")
	}
	video := media.VideoInput{
		Model:           "grok-imagine-video-1.5",
		Prompt:          []content.Block{content.TextBlock("Animate")},
		ReferenceFrames: []content.Block{bad},
	}
	if _, err := videoWireBody(video); err == nil {
		t.Fatal("non-image inline bytes were accepted as video frame")
	}
}
