package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func newMediaFixture(t *testing.T, server *httptest.Server) *MediaClient {
	t.Helper()
	p, err := NewMedia(MediaConfig{Provider: "google-test", APIKey: "fixture-key",
		ImageEndpoint:       server.URL + "/v1/models/{model}:generateContent",
		VideoEndpoint:       server.URL + "/v1beta/models/{model}:predictLongRunning",
		UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMediaImagePrepareAndSend(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/models/gemini-image:generateContent" ||
			r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
			t.Errorf("unexpected image request: %s %s", r.Method, r.URL)
		}
		var body struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			GenerationConfig struct {
				ResponseModalities []string `json:"responseModalities"`
				ImageConfig        struct {
					AspectRatio string `json:"aspectRatio"`
					ImageSize   string `json:"imageSize"`
				} `json:"imageConfig"`
			} `json:"generationConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Contents[0].Parts[0].Text != "a cat" ||
			body.GenerationConfig.ImageConfig.AspectRatio != "16:9" ||
			body.GenerationConfig.ImageConfig.ImageSize != "2K" ||
			len(body.GenerationConfig.ResponseModalities) != 1 ||
			body.GenerationConfig.ResponseModalities[0] != "IMAGE" {
			t.Errorf("unexpected image body: %#v", body)
		}
		fmt.Fprintf(w, `{"responseId":"image-1","modelVersion":"gemini-image-v1","usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":1290},"candidates":[{"content":{"parts":[{"text":"here"},{"inlineData":{"mimeType":"image/png","data":"%s"}}]}}]}`, base64.StdEncoding.EncodeToString([]byte("png-data")))
	}))
	defer server.Close()
	p := newMediaFixture(t, server)
	in := media.ImageInput{Model: "gemini-image", Operation: media.ImageGenerate,
		Prompt:   []content.Block{content.TextBlock("a cat")},
		Settings: media.ImageSettings{AspectRatio: "16:9", Resolution: "2K"}}
	request, err := p.PrepareImage(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || len(request.Snapshot().Unverified) == 0 {
		t.Fatalf("preparation dispatched or lost uncertainty: sends=%d, request=%+v", sends, request.Snapshot())
	}
	in.Prompt[0].Text.Text = "changed"
	result, err := p.SendImage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 1 || result.Served.Model != "gemini-image-v1" ||
		!strings.Contains(string(result.RawUsage), `"promptTokenCount":12`) ||
		len(result.Content) != 2 || string(result.Content[1].Image.Source.Data) != "png-data" {
		t.Fatalf("unexpected image result: sends=%d, result=%+v", sends, result)
	}
}

func TestMediaVeoJobLifecycle(t *testing.T) {
	var submitted, polled, fetched int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
			t.Error("missing API key")
		}
		switch r.URL.Path {
		case "/v1beta/models/veo:predictLongRunning":
			submitted++
			if r.Method != http.MethodPost {
				t.Errorf("submission used %s", r.Method)
			}
			var body struct {
				Instances []struct {
					Prompt string `json:"prompt"`
				} `json:"instances"`
				Parameters struct {
					DurationSeconds int    `json:"durationSeconds"`
					AspectRatio     string `json:"aspectRatio"`
				} `json:"parameters"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Instances[0].Prompt != "a sunrise" || body.Parameters.DurationSeconds != 8 || body.Parameters.AspectRatio != "16:9" {
				t.Errorf("unexpected Veo submission: %+v", body)
			}
			io.WriteString(w, `{"name":"models/veo/operations/job-1"}`)
		case "/v1beta/models/veo/operations/job-1":
			polled++
			if polled == 1 {
				io.WriteString(w, `{"name":"models/veo/operations/job-1","done":false}`)
			} else {
				fmt.Fprintf(w, `{"name":"models/veo/operations/job-1","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":%q,"mimeType":"video/mp4"}}]}}}`, server.URL+"/v1beta/files/output-1")
			}
		case "/v1beta/files/output-1:download":
			fetched++
			if r.URL.Query().Get("alt") != "media" {
				t.Errorf("download lacks alt=media: %s", r.URL)
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "video-data")
		default:
			t.Errorf("unexpected Veo path %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := newMediaFixture(t, server)
	request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "veo",
		Prompt:   []content.Block{content.TextBlock("a sunrise")},
		Settings: media.VideoSettings{Duration: 8 * time.Second, AspectRatio: "16:9"}})
	if err != nil {
		t.Fatal(err)
	}
	if submitted != 0 {
		t.Fatal("preparation submitted a job")
	}
	submission, err := p.SubmitVideo(context.Background(), request)
	if err != nil || submission.State != media.SubmissionAccepted || submission.Job == nil {
		t.Fatalf("submission: %+v, %v", submission, err)
	}
	first, err := p.PollVideo(context.Background(), *submission.Job)
	if err != nil || first.State != media.JobPending {
		t.Fatalf("pending poll: %+v, %v", first, err)
	}
	second, err := p.PollVideo(context.Background(), *submission.Job)
	if err != nil || second.State != media.JobSucceeded || len(second.Content) != 1 {
		t.Fatalf("completed poll: %+v, %v", second, err)
	}
	result, err := p.FetchVideo(context.Background(), second)
	if err != nil || string(result.Content[0].Video.Source.Data) != "video-data" ||
		submitted != 1 || polled != 2 || fetched != 1 {
		t.Fatalf("fetch: %+v, %v; calls=%d/%d/%d", result, err, submitted, polled, fetched)
	}
}

type failingMediaTransport struct{}

func (failingMediaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection dropped after send")
}

func TestMediaVeoUnknownSubmission(t *testing.T) {
	p, err := NewMedia(MediaConfig{Provider: "google-test", APIKey: "fixture-key",
		Client:              &http.Client{Transport: failingMediaTransport{}},
		UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "veo",
		Prompt: []content.Block{content.TextBlock("a sunrise")}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := p.SubmitVideo(context.Background(), request)
	var mediaErr *media.Error
	if submission.State != media.SubmissionUnknown || !errors.As(err, &mediaErr) ||
		mediaErr.Category != media.ErrorTransport {
		t.Fatalf("transport outcome: %+v, %v", submission, err)
	}
}

func TestMediaInlineEditAndInitialFrame(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newMediaFixture(t, server)
	image := content.ImageBlock(content.Source{Data: []byte("image-bytes"), MIMEType: "image/png"})
	edit, err := p.PrepareImage(context.Background(), media.ImageInput{Model: "gemini-image",
		Operation: media.ImageEdit, Prompt: []content.Block{content.TextBlock("change color")},
		ReferenceImages: []content.Block{image}})
	if err != nil {
		t.Fatal(err)
	}
	var imageBody struct {
		Contents []struct {
			Parts []struct {
				InlineData *struct {
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(edit.Body(), &imageBody); err != nil {
		t.Fatal(err)
	}
	if len(imageBody.Contents[0].Parts) != 2 || imageBody.Contents[0].Parts[1].InlineData == nil ||
		imageBody.Contents[0].Parts[1].InlineData.Data != base64.StdEncoding.EncodeToString([]byte("image-bytes")) {
		t.Fatalf("edit did not encode reference image: %+v", imageBody)
	}
	video, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "veo",
		ReferenceFrames: []content.Block{image}})
	if err != nil {
		t.Fatal(err)
	}
	var videoBody struct {
		Instances []struct {
			Image struct {
				InlineData struct {
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"image"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(video.Body(), &videoBody); err != nil {
		t.Fatal(err)
	}
	if videoBody.Instances[0].Image.InlineData.Data != base64.StdEncoding.EncodeToString([]byte("image-bytes")) {
		t.Fatalf("Veo did not encode initial frame: %+v", videoBody)
	}
}

func TestMediaSubmissionHTTPOutcome(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   media.SubmissionState
	}{{http.StatusBadRequest, media.SubmissionRejected},
		{http.StatusTooManyRequests, media.SubmissionUnknown},
		{http.StatusInternalServerError, media.SubmissionUnknown}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"message":"failed"}}`)
			}))
			defer server.Close()
			p := newMediaFixture(t, server)
			request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "veo",
				Prompt: []content.Block{content.TextBlock("a sunrise")}})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := p.SubmitVideo(context.Background(), request)
			var mediaErr *media.Error
			if outcome.State != tc.want || outcome.Job != nil ||
				!errors.As(err, &mediaErr) || mediaErr.Status != tc.status {
				t.Fatalf("status %d: outcome=%+v, error=%v", tc.status, outcome, err)
			}
		})
	}
}

func TestMediaModelEvidenceControlsPreparation(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	imageEndpoint := server.URL + "/v1/models/{model}:generateContent"
	info := media.ModelInfo{Image: &media.ImageCapabilities{Generate: model.SupportUnsupported},
		Evidence: model.ModelEvidence{Model: "gemini-image", API: ImageAPI,
			Endpoint: server.URL + "/v1/models/gemini-image:generateContent",
			Owner:    "test", Source: "fixture", Method: model.EvidenceFixture,
			QualifiedAt: time.Now()}}
	p, err := NewMedia(MediaConfig{Provider: "google-test", APIKey: "fixture-key",
		ImageEndpoint:       imageEndpoint,
		VideoEndpoint:       server.URL + "/v1beta/models/{model}:predictLongRunning",
		ImageModels:         map[string]media.ModelInfo{"gemini-image": info},
		UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.PrepareImage(context.Background(), media.ImageInput{Model: "gemini-image",
		Operation: media.ImageGenerate, Prompt: []content.Block{content.TextBlock("a cat")}})
	var mediaErr *media.Error
	if !errors.As(err, &mediaErr) || mediaErr.Category != media.ErrorUnsupported {
		t.Fatalf("known rejection was not enforced: %v", err)
	}
	info.Evidence.Endpoint = server.URL + "/v1/models/other:generateContent"
	_, err = NewMedia(MediaConfig{Provider: "google-test", APIKey: "fixture-key",
		ImageEndpoint: imageEndpoint,
		VideoEndpoint: server.URL + "/v1beta/models/{model}:predictLongRunning",
		ImageModels:   map[string]media.ModelInfo{"gemini-image": info}})
	if err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("mismatched model evidence accepted: %v", err)
	}
}

func TestMediaRejectsUnsupportedAndForeignOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"models/veo/operations/job-1","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://evil.example/v1beta/files/output"}}]}}}`)
	}))
	defer server.Close()
	p := newMediaFixture(t, server)
	_, err := p.PrepareImage(context.Background(), media.ImageInput{Model: "gemini-image",
		Operation: media.ImageEdit, Prompt: []content.Block{content.TextBlock("edit")},
		ReferenceImages: []content.Block{content.ImageBlock(content.Source{Data: []byte("image"), MIMEType: "image/png"})},
		Mask: func() *content.Block {
			b := content.ImageBlock(content.Source{Data: []byte("mask"), MIMEType: "image/png"})
			return &b
		}()})
	if err == nil || !strings.Contains(err.Error(), "mask") {
		t.Fatalf("expected explicit mask rejection, got %v", err)
	}
	_, err = p.PrepareVideo(context.Background(), media.VideoInput{Model: "veo",
		Prompt: []content.Block{content.TextBlock("a sunrise")},
		ReferenceFrames: []content.Block{
			content.ImageBlock(content.Source{Data: []byte("first"), MIMEType: "image/png"}),
			content.ImageBlock(content.Source{Data: []byte("second"), MIMEType: "image/png"}),
		}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected explicit reference-frame rejection, got %v", err)
	}
	job := media.Job{ID: "models/veo/operations/job-1", Kind: media.KindVideo, Origin: p.videoOrigin("veo")}
	_, err = p.PollVideo(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected foreign output URI rejection, got %v", err)
	}
}
