package imagine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

func TestVideo15AliasAccepts1080pGeneration(t *testing.T) {
	for _, name := range []string{
		"grok-imagine-video-1.5",
		"grok-imagine-video-1.5-preview",
		"grok-imagine-video-1.5-2026-05-30",
	} {
		t.Run(name, func(t *testing.T) {
			input := media.VideoInput{
				Model: name, Prompt: []content.Block{content.TextBlock("A lake")},
				Settings: media.VideoSettings{Resolution: "1080p"},
			}
			if _, err := videoWireBody(input); err != nil {
				t.Fatal(err)
			}
			if _, err := videoWireBodyWithOptions(input, VideoOptions{
				ReferenceImages: []content.Block{content.ImageBlock(content.Source{URL: "https://example.test/guide.png"})},
			}); err == nil {
				t.Fatal("reference-to-video accepted 1080p")
			}
		})
	}
}

func TestVideoOptionsCaptureDocumentedGenerationModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preparation unexpectedly made a network request")
	}))
	defer server.Close()
	client := testClient(t, server.URL+"/v1", server.Client(), model.CapabilityAdjust)
	last := content.ImageBlock(content.Source{URL: "https://example.test/last.png"})
	silent := false
	input := media.VideoInput{
		Model: "grok-imagine-video-1.5",
		ReferenceFrames: []content.Block{
			content.ImageBlock(content.Source{URL: "https://example.test/first.png"}),
		},
		Settings: media.VideoSettings{Duration: 6 * time.Second, Resolution: "720p"},
	}
	options := VideoOptions{
		ReferenceImages: []content.Block{content.ImageBlock(content.Source{URL: "https://example.test/guide.png"})},
		LastFrame:       &last,
		Keyframes: []VideoKeyframe{
			{Image: content.ImageBlock(content.Source{URL: "https://example.test/middle.png"}), Timestamp: 7 * time.Second / 3},
		},
		ReferenceVoices: []string{"eve"},
		GenerateAudio:   &silent,
	}
	request, err := client.PrepareVideoWithOptions(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Image           map[string]string   `json:"image"`
		ReferenceImages []map[string]string `json:"reference_images"`
		LastFrame       map[string]string   `json:"last_frame"`
		Keyframes       []struct {
			Image     map[string]string `json:"image"`
			Timestamp float64           `json:"timestamp_s"`
		} `json:"keyframes"`
		ReferenceAudios []map[string]string `json:"reference_audios"`
		GenerateAudio   *bool               `json:"generate_audio"`
	}
	if err := json.Unmarshal(request.Body(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Image["url"] != "https://example.test/first.png" ||
		len(wire.ReferenceImages) != 1 || wire.ReferenceImages[0]["url"] != "https://example.test/guide.png" ||
		wire.LastFrame["url"] != "https://example.test/last.png" ||
		len(wire.Keyframes) != 1 || wire.Keyframes[0].Timestamp != 7.0/3 ||
		len(wire.ReferenceAudios) != 1 || wire.ReferenceAudios[0]["voice_id"] != "eve" ||
		wire.GenerateAudio == nil || *wire.GenerateAudio {
		t.Fatalf("unexpected video wire body: %s", request.Body())
	}
	snapshot := request.Snapshot()
	if len(snapshot.ProviderOptions) == 0 || !json.Valid(snapshot.ProviderOptions) {
		t.Fatal("provider options were not recorded")
	}
	options.ReferenceImages[0].Image.Source.URL = "https://example.test/changed.png"
	if string(request.Body()) != string(snapshot.Body) {
		t.Fatal("caller mutation changed prepared body")
	}
}

func TestVideoOptionsRejectInvalidModeCombinations(t *testing.T) {
	frame := content.ImageBlock(content.Source{URL: "https://example.test/frame.png"})
	base := media.VideoInput{
		Model:    "grok-imagine-video-1.5",
		Prompt:   []content.Block{content.TextBlock("A lake")},
		Settings: media.VideoSettings{Duration: 6 * time.Second},
	}
	for _, tc := range []struct {
		name   string
		change func(*media.VideoInput, *VideoOptions)
	}{
		{"empty input", func(in *media.VideoInput, _ *VideoOptions) { in.Prompt = nil }},
		{"classic last frame", func(in *media.VideoInput, opts *VideoOptions) {
			in.Model = "grok-imagine-video"
			opts.LastFrame = &frame
		}},
		{"classic first and reference", func(in *media.VideoInput, opts *VideoOptions) {
			in.Model = "grok-imagine-video"
			in.ReferenceFrames = []content.Block{frame}
			opts.ReferenceImages = []content.Block{frame}
		}},
		{"too many references", func(_ *media.VideoInput, opts *VideoOptions) {
			opts.ReferenceImages = make([]content.Block, 8)
			for i := range opts.ReferenceImages {
				opts.ReferenceImages[i] = frame
			}
		}},
		{"keyframe outside clip", func(_ *media.VideoInput, opts *VideoOptions) {
			opts.Keyframes = []VideoKeyframe{{Image: frame, Timestamp: 6 * time.Second}}
		}},
		{"keyframe off grid", func(_ *media.VideoInput, opts *VideoOptions) {
			opts.Keyframes = []VideoKeyframe{{Image: frame, Timestamp: 400 * time.Millisecond}}
		}},
		{"keyframes overlap", func(_ *media.VideoInput, opts *VideoOptions) {
			opts.Keyframes = []VideoKeyframe{{Image: frame, Timestamp: time.Second}, {Image: frame, Timestamp: time.Second}}
		}},
		{"reference URL userinfo", func(_ *media.VideoInput, opts *VideoOptions) {
			opts.ReferenceImages = []content.Block{content.ImageBlock(content.Source{URL: "https://user:secret@example.test/frame.png"})}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base.Clone()
			opts := VideoOptions{}
			tc.change(&in, &opts)
			if _, err := videoWireBodyWithOptions(in, opts); err == nil {
				t.Fatal("invalid video mode was accepted")
			}
		})
	}
}

func TestVideoEditAndExtendUseSeparateJobEndpoints(t *testing.T) {
	for _, tc := range []struct {
		operation media.VideoOperation
		path      string
		duration  time.Duration
	}{
		{media.VideoEdit, "/v1/videos/edits", 0},
		{media.VideoExtend, "/v1/videos/extensions", 5 * time.Second},
	} {
		t.Run(string(tc.operation), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != tc.path || r.Method != http.MethodPost {
					t.Errorf("unexpected submit: %s %s", r.Method, r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				var wire struct {
					Video    map[string]string `json:"video"`
					Duration int               `json:"duration"`
				}
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Error(err)
				}
				if wire.Video["url"] != "https://example.test/source.mp4" || wire.Duration != int(tc.duration/time.Second) {
					t.Errorf("unexpected transform body: %s", body)
				}
				_, _ = io.WriteString(w, `{"request_id":"transform-1"}`)
			}))
			defer server.Close()
			client := testClient(t, server.URL+"/v1", server.Client(), model.CapabilityAdjust)
			source := content.VideoBlock(content.Source{URL: "https://example.test/source.mp4"})
			source.Video.Duration = 5 * time.Second
			input := media.VideoInput{
				Model: "grok-imagine-video-1.5", Operation: tc.operation,
				Prompt:   []content.Block{content.TextBlock("Change the scene")},
				Settings: media.VideoSettings{Duration: tc.duration},
			}
			request, err := client.PrepareVideoWithOptions(context.Background(), input, VideoOptions{SourceVideo: &source})
			if err != nil {
				t.Fatal(err)
			}
			if len(request.Snapshot().Unverified) == 0 {
				t.Fatal("unverified transform capability was not recorded")
			}
			submission, err := client.SubmitVideo(context.Background(), request)
			if err != nil || submission.State != media.SubmissionAccepted || calls != 1 {
				t.Fatalf("submission = %+v, err = %v, calls = %d", submission, err, calls)
			}
		})
	}
}

func TestVideoTransformRejectsUnsupportedSettings(t *testing.T) {
	source := content.VideoBlock(content.Source{URL: "https://example.test/source.mp4"})
	base := media.VideoInput{
		Model: "grok-imagine-video", Operation: media.VideoEdit,
		Prompt: []content.Block{content.TextBlock("Add a hat")},
	}
	for _, tc := range []struct {
		name   string
		change func(*media.VideoInput, *VideoOptions)
	}{
		{"edit duration", func(in *media.VideoInput, _ *VideoOptions) { in.Settings.Duration = time.Second }},
		{"edit resolution", func(in *media.VideoInput, _ *VideoOptions) { in.Settings.Resolution = "720p" }},
		{"extension duration", func(in *media.VideoInput, _ *VideoOptions) {
			in.Operation = media.VideoExtend
			in.Settings.Duration = time.Second
		}},
		{"extension aspect ratio", func(in *media.VideoInput, _ *VideoOptions) {
			in.Operation = media.VideoExtend
			in.Settings.AspectRatio = "16:9"
		}},
		{"source MIME is not mp4", func(_ *media.VideoInput, opts *VideoOptions) {
			bad := content.VideoBlock(content.Source{URL: "https://example.test/opaque", MIMEType: "video/quicktime"})
			opts.SourceVideo = &bad
		}},
		{"source duration too long", func(in *media.VideoInput, opts *VideoOptions) {
			in.Operation = media.VideoEdit
			bad := source.Clone()
			bad.Video.Duration = 9 * time.Second
			opts.SourceVideo = &bad
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base.Clone()
			opts := VideoOptions{SourceVideo: &source}
			tc.change(&in, &opts)
			if _, err := videoWireBodyWithOptions(in, opts); err == nil {
				t.Fatal("unsupported transform request was accepted")
			}
		})
	}
}

func TestVideoTransformAcceptsOpaqueSignedMP4URL(t *testing.T) {
	source := content.VideoBlock(content.Source{
		URL:      "https://cdn.example.test/asset/abc123?token=signed",
		MIMEType: "video/mp4",
	})
	input := media.VideoInput{
		Model: "grok-imagine-video", Operation: media.VideoEdit,
		Prompt: []content.Block{content.TextBlock("Add rain")},
	}
	body, err := videoWireBodyWithOptions(input, VideoOptions{SourceVideo: &source})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Video map[string]string `json:"video"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Video["url"] != source.Video.Source.URL {
		t.Fatalf("opaque source URL was not preserved: %s, %v", body, err)
	}
}

func TestVideoReferenceAndSourceFilesUseBoundFilesOrigin(t *testing.T) {
	client, err := New(Config{
		Provider: "test-grok", Endpoint: "https://example.test/v1", APIKey: "key",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	image := content.ImageBlock(content.Source{ID: "file_image", Origin: client.FileOrigin(), MIMEType: "image/png"})
	input := media.VideoInput{Model: "grok-imagine-video-1.5", Prompt: []content.Block{content.TextBlock("Move")}}
	request, err := client.PrepareVideoWithOptions(context.Background(), input, VideoOptions{ReferenceImages: []content.Block{image}})
	if err != nil {
		t.Fatal(err)
	}
	var reference struct {
		ReferenceImages []map[string]string `json:"reference_images"`
	}
	if err := json.Unmarshal(request.Body(), &reference); err != nil ||
		len(reference.ReferenceImages) != 1 || reference.ReferenceImages[0]["file_id"] != "file_image" {
		t.Fatalf("reference file was not encoded: %s, %v", request.Body(), err)
	}
	source := content.VideoBlock(content.Source{ID: "file_video", Origin: client.FileOrigin(), MIMEType: "video/mp4"})
	input.Operation = media.VideoEdit
	request, err = client.PrepareVideoWithOptions(context.Background(), input, VideoOptions{SourceVideo: &source})
	if err != nil {
		t.Fatal(err)
	}
	var edit struct {
		Video map[string]string `json:"video"`
	}
	if err := json.Unmarshal(request.Body(), &edit); err != nil || edit.Video["file_id"] != "file_video" {
		t.Fatalf("source video file was not encoded: %s, %v", request.Body(), err)
	}
	bad := source.Clone()
	bad.Video.Source.Origin.Scope = "wrong"
	if _, err := client.PrepareVideoWithOptions(context.Background(), input, VideoOptions{SourceVideo: &bad}); err == nil {
		t.Fatal("source video file from another scope was accepted")
	}
}
