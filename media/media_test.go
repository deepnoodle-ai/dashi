package media

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

func TestImageInputValidateEditRequiresReference(t *testing.T) {
	input := ImageInput{
		Model:     "image-model",
		Operation: ImageEdit,
		Prompt:    []content.Block{content.TextBlock("remove the background")},
	}
	if err := input.Validate(); err == nil {
		t.Fatal("accepted an image edit without a reference image")
	}
	input.ReferenceImages = []content.Block{content.ImageBlock(content.Source{
		Data:     []byte{1},
		MIMEType: "image/png",
	})}
	if err := input.Validate(); err != nil {
		t.Fatalf("valid image edit rejected: %v", err)
	}
	input.Prompt = nil
	if err := input.Validate(); err != nil {
		t.Fatalf("reference-only image edit rejected: %v", err)
	}
}

func TestImageInputCloneCopiesReferenceBytes(t *testing.T) {
	input := ImageInput{
		ReferenceImages: []content.Block{content.ImageBlock(content.Source{
			Data:     []byte{1},
			MIMEType: "image/png",
		})},
	}
	clone := input.Clone()
	clone.ReferenceImages[0].Image.Source.Data[0] = 9
	if input.ReferenceImages[0].Image.Source.Data[0] != 1 {
		t.Fatal("image reference bytes were shared")
	}
}

func TestSubmissionValidateKeepsUnknownDistinctFromAccepted(t *testing.T) {
	job := Job{
		ID:   "job-1",
		Kind: KindVideo,
		Origin: content.Origin{
			Provider: "test",
			API:      "videos",
			Model:    "video-model",
			Scope:    "account",
		},
	}
	tests := []struct {
		name       string
		submission Submission
		valid      bool
	}{
		{
			name:       "unknown receipt",
			submission: Submission{State: SubmissionUnknown},
			valid:      true,
		},
		{
			name: "accepted handle",
			submission: Submission{
				State: SubmissionAccepted,
				Job:   &job,
			},
			valid: true,
		},
		{
			name:       "accepted without handle",
			submission: Submission{State: SubmissionAccepted},
		},
		{
			name: "unknown with handle",
			submission: Submission{
				State: SubmissionUnknown,
				Job:   &job,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.submission.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("submission validity=%t, want %t: %v", err == nil, tt.valid, err)
			}
		})
	}
}

func TestSubmissionCloneCopiesJobExpiry(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	submission := Submission{
		State: SubmissionAccepted,
		Job: &Job{
			ID:        "job-1",
			ExpiresAt: &expires,
		},
	}
	clone := submission.Clone()
	clone.Job.ID = "job-2"
	*clone.Job.ExpiresAt = expires.Add(time.Hour)
	if submission.Job.ID != "job-1" {
		t.Fatal("job handle was shared")
	}
	if !submission.Job.ExpiresAt.Equal(expires) {
		t.Fatal("job expiry was shared")
	}
}

func TestSubmissionKeepsInitialResponseFacts(t *testing.T) {
	job := Job{ID: "files/video-1", Kind: KindVideo, Origin: content.Origin{
		Provider: "google", API: "google.interactions", Model: "omni", Scope: "account",
	}}
	submission := Submission{State: SubmissionAccepted, Job: &job,
		ResponseID: "interaction-1", Served: model.ModelRef{Provider: "google", Model: "omni"},
		RawUsage: json.RawMessage(`{"total_tokens":12}`)}
	if err := submission.Validate(); err != nil {
		t.Fatalf("valid submission rejected: %v", err)
	}
	copy := submission.Clone()
	copy.RawUsage[0] = '['
	if submission.RawUsage[0] != '{' {
		t.Fatal("submission usage bytes were shared")
	}
	copy.Served.Provider = "other"
	if err := copy.Validate(); err == nil {
		t.Fatal("accepted a served model from another provider")
	}
	unknown := Submission{State: SubmissionUnknown, ResponseID: "interaction-2",
		Served:   model.ModelRef{Provider: "google", Model: "omni"},
		RawUsage: json.RawMessage(`{"total_tokens":3}`)}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("observed facts without a handle were lost: %v", err)
	}
}

func TestResultValidateRequiresCoherentServedIdentity(t *testing.T) {
	result := Result{
		Kind: KindImage,
		Content: []content.Block{content.ImageBlock(content.Source{
			Data:     []byte{1},
			MIMEType: "image/png",
		})},
		Requested: model.ModelRef{Provider: "test", Model: "requested"},
		API:       "images",
		RawUsage:  json.RawMessage(`{"input_tokens":12}`),
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid image result rejected: %v", err)
	}
	result.Content = append(result.Content, content.TextBlock("generated image"))
	if err := result.Validate(); err != nil {
		t.Fatalf("result with ancillary text rejected: %v", err)
	}
	result.Served = model.ModelRef{Model: "served"}
	if err := result.Validate(); err == nil {
		t.Fatal("accepted a served model without its configured provider")
	}
}

func TestResultCloneCopiesRawUsage(t *testing.T) {
	result := Result{RawUsage: json.RawMessage(`{"input_tokens":12}`)}
	clone := result.Clone()
	clone.RawUsage[0] = '['
	if result.RawUsage[0] != '{' {
		t.Fatal("provider usage bytes were shared")
	}
}

func TestStoredOutputPreservesProviderFileAndPublicURL(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	result := Result{Kind: KindImage,
		Content:   []content.Block{content.ImageBlock(content.Source{URL: "https://temporary.example/image.png"})},
		Requested: model.ModelRef{Provider: "grok", Model: "imagine"}, API: "xai.images",
		StoredOutputs: []StoredOutput{{ContentIndex: 0,
			File: content.Source{ID: "file-1", Origin: content.Origin{
				Provider: "grok", API: "xai.files", Scope: "account"}},
			Filename: "image.png", PublicURL: &content.Source{
				URL: "https://public.example/image.png", ExpiresAt: &expires}}},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid stored output rejected: %v", err)
	}
	copy := result.Clone()
	copy.StoredOutputs[0].File.ID = "other"
	*copy.StoredOutputs[0].PublicURL.ExpiresAt = expires.Add(time.Hour)
	if result.StoredOutputs[0].File.ID != "file-1" ||
		!result.StoredOutputs[0].PublicURL.ExpiresAt.Equal(expires) {
		t.Fatal("stored output was shared")
	}
	result.StoredOutputs[0].PublicURLError = "quota"
	if err := result.Validate(); err == nil {
		t.Fatal("accepted a public URL and its failure together")
	}
	result.StoredOutputs[0].PublicURL = nil
	if err := result.Validate(); err != nil {
		t.Fatalf("partial URL failure lost stored file: %v", err)
	}
	result.StoredOutputs[0].ContentIndex = 1
	if err := result.Validate(); err == nil {
		t.Fatal("accepted a stored output without a corresponding block")
	}
}

func TestModelInfoCloneCopiesCapabilityLists(t *testing.T) {
	info := ModelInfo{
		Image: &ImageCapabilities{
			Sizes: []Size{{Width: 1024, Height: 1024}},
		},
	}
	clone := info.Clone()
	clone.Image.Sizes[0].Width = 512
	if info.Image.Sizes[0].Width != 1024 {
		t.Fatal("image size constraints were shared")
	}
}
