package media

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

func imageSnapshot() ImageRequestSnapshot {
	return ImageRequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.images",
			Model:    "image-model",
			Scope:    "account",
		},
		Input: ImageInput{
			Model:     "image-model",
			Operation: ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("a mountain")},
		},
		Body:        []byte(`{"model":"image-model","prompt":"a mountain"}`),
		ContentType: "application/json",
	}
}

func videoSnapshot() VideoRequestSnapshot {
	return VideoRequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.videos",
			Model:    "video-model",
			Scope:    "account",
		},
		Input: VideoInput{
			Model:  "video-model",
			Prompt: []content.Block{content.TextBlock("a mountain")},
		},
		Body:        []byte(`{"model":"video-model","prompt":"a mountain"}`),
		ContentType: "application/json",
	}
}

func TestImageRequestOwnsPreparationBytes(t *testing.T) {
	snapshot := imageSnapshot()
	request, err := NewImageRequest(snapshot)
	if err != nil {
		t.Fatalf("valid image preparation rejected: %v", err)
	}
	snapshot.Body[0] = '['
	snapshot.Input.Prompt[0].Text.Text = "changed"
	if bytes.Equal(request.Body(), snapshot.Body) {
		t.Fatal("prepared image body shared caller bytes")
	}
	if request.Input().Prompt[0].Text.Text != "a mountain" {
		t.Fatal("prepared image prompt shared caller storage")
	}
	view := request.Snapshot()
	view.Body[0] = '['
	view.Input.Prompt[0].Text.Text = "changed again"
	if request.Body()[0] != '{' {
		t.Fatal("image snapshot mutation changed prepared body")
	}
	if request.Input().Prompt[0].Text.Text != "a mountain" {
		t.Fatal("image snapshot mutation changed prepared input")
	}
}

func TestImageRequestRequiresMultipartBoundary(t *testing.T) {
	snapshot := imageSnapshot()
	snapshot.ContentType = "multipart/form-data"
	snapshot.Body = []byte("binary")
	if _, err := NewImageRequest(snapshot); err == nil {
		t.Fatal("accepted multipart preparation without a boundary")
	}
	snapshot.ContentType = "multipart/form-data; boundary=abc"
	if _, err := NewImageRequest(snapshot); err != nil {
		t.Fatalf("valid multipart preparation rejected: %v", err)
	}
}

func TestVideoRequestRecordsUnknownCapabilityPolicy(t *testing.T) {
	snapshot := videoSnapshot()
	snapshot.Unverified = []string{"video.generate"}
	if _, err := NewVideoRequest(snapshot); err == nil {
		t.Fatal("accepted unverified video support without explicit allowance")
	}
	snapshot.UnknownCapabilities = model.UnknownAllow
	request, err := NewVideoRequest(snapshot)
	if err != nil {
		t.Fatalf("explicitly allowed unknown support rejected: %v", err)
	}
	if request.Snapshot().Unverified[0] != "video.generate" {
		t.Fatal("prepared video request lost unverified support")
	}
}

func TestVideoRequestRequiresRecordedSettingAdjustment(t *testing.T) {
	snapshot := videoSnapshot()
	snapshot.Effective.Duration = 8 * time.Second
	if _, err := NewVideoRequest(snapshot); err == nil {
		t.Fatal("accepted changed settings without an adjustment")
	}
	snapshot.Adjustments = []Adjustment{{
		Setting:   "duration",
		Effective: "8s",
		Reason:    "provider default",
	}}
	if _, err := NewVideoRequest(snapshot); err != nil {
		t.Fatalf("documented provider default rejected: %v", err)
	}
}

func TestJobPollCloneCopiesOutputReference(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	poll := JobPoll{
		Job: Job{
			ID:   "job-1",
			Kind: KindVideo,
			Origin: content.Origin{
				Provider: "test",
				API:      "test.videos",
				Model:    "video-model",
				Scope:    "account",
			},
		},
		State:    JobSucceeded,
		RawUsage: json.RawMessage(`{"cost_in_usd_ticks":123}`),
		Content: []content.Block{content.VideoBlock(content.Source{
			URL:       "https://example.test/video.mp4",
			ExpiresAt: &expires,
		})},
	}
	if err := poll.Validate(); err != nil {
		t.Fatalf("valid successful poll rejected: %v", err)
	}
	clone := poll.Clone()
	*clone.Content[0].Video.Source.ExpiresAt = expires.Add(time.Hour)
	clone.RawUsage[0] = '['
	if !poll.Content[0].Video.Source.ExpiresAt.Equal(expires) {
		t.Fatal("polled output expiry was shared")
	}
	if poll.RawUsage[0] != '{' {
		t.Fatal("polled usage bytes were shared")
	}
}
