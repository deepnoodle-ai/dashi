//go:build live

package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/media"
	"github.com/deepnoodle-ai/kernel/model"
)

// Run with KERNEL_LIVE_VIDEO_TEST=1, GEMINI_API_KEY, and GEMINI_VIDEO_MODEL.
// This makes one paid video submission, then polls and downloads that job.
// KERNEL_LIVE_VEO_JOB_ID resumes an existing job without submitting another.
func TestLiveVeoVideoJob(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_VIDEO_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_VIDEO_TEST=1 to make a paid Veo call")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_VIDEO_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_VIDEO_MODEL are required")
	}
	client, err := NewMedia(MediaConfig{
		APIKey:           key,
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	job := media.Job{
		ID:     os.Getenv("KERNEL_LIVE_VEO_JOB_ID"),
		Kind:   media.KindVideo,
		Origin: client.videoOrigin(modelName),
	}
	if job.ID == "" {
		request, err := client.PrepareVideo(
			ctx,
			media.VideoInput{
				Model: modelName,
				Prompt: []content.Block{
					content.TextBlock("A single blue ceramic cup slowly rotates on a plain white table. Steady camera, no people, no text."),
				},
				Settings: media.VideoSettings{
					Duration:    4 * time.Second,
					Resolution:  "720p",
					AspectRatio: "16:9",
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		submission, err := client.SubmitVideo(ctx, request)
		if err != nil {
			t.Fatalf("submit (state %s): %v", submission.State, err)
		}
		if submission.State != media.SubmissionAccepted || submission.Job == nil {
			t.Fatalf("Veo did not return a job: %+v", submission)
		}
		job = *submission.Job
	}
	t.Logf("polling Veo job: %s", job.ID)
	for {
		poll, err := client.PollVideo(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		switch poll.State {
		case media.JobPending:
			select {
			case <-ctx.Done():
				t.Fatalf("Veo job still pending: %v", ctx.Err())
			case <-time.After(10 * time.Second):
			}
		case media.JobSucceeded:
			result, err := client.FetchVideo(ctx, poll)
			if err != nil {
				t.Fatal(err)
			}
			checkLiveVideo(t, result, modelName, VideoAPI, "veo")
			return
		default:
			t.Fatalf("Veo job ended in %s: %s", poll.State, poll.Failure)
		}
	}
}

// Run with KERNEL_LIVE_VIDEO_TEST=1, GEMINI_API_KEY, and GEMINI_OMNI_MODEL.
// This makes one paid synchronous Interactions video call.
func TestLiveOmniVideoCall(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_VIDEO_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_VIDEO_TEST=1 to make a paid Omni call")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_OMNI_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_OMNI_MODEL are required")
	}
	client, err := NewInteractionsMedia(InteractionsMediaConfig{
		APIKey:           key,
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	request, err := client.PrepareVideo(
		ctx,
		media.VideoInput{
			Model: modelName,
			Prompt: []content.Block{
				content.TextBlock("A blue ceramic cup slowly rotates on a plain white table. Steady camera, no people, no text."),
			},
			Settings: media.VideoSettings{Resolution: "360p", AspectRatio: "16:9"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SendVideo(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	checkLiveVideo(t, result, modelName, InteractionsAPI, "omni")
}

// KERNEL_LIVE_OMNI_JOB_ID resumes a Files job without another paid submission.
func TestLiveOmniVideoJob(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_VIDEO_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_VIDEO_TEST=1 to make a paid Omni call")
	}
	key, modelName := os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_OMNI_MODEL")
	if key == "" || modelName == "" {
		t.Fatal("GEMINI_API_KEY and GEMINI_OMNI_MODEL are required")
	}
	client, err := NewInteractionsVideoJobs(InteractionsMediaConfig{
		APIKey:           key,
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	job := media.Job{
		ID:     os.Getenv("KERNEL_LIVE_OMNI_JOB_ID"),
		Kind:   media.KindVideo,
		Origin: client.client.origin(modelName),
	}
	if job.ID == "" {
		request, err := client.PrepareVideo(
			ctx,
			media.VideoInput{
				Model: modelName,
				Prompt: []content.Block{
					content.TextBlock("A blue ceramic cup slowly rotates on a plain white table. Steady camera, no people, no text."),
				},
				Settings: media.VideoSettings{Resolution: "360p", AspectRatio: "16:9"},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		submission, err := client.SubmitVideo(ctx, request)
		if err != nil {
			t.Fatalf(
				"submit (state %s, response %s): %s",
				submission.State,
				submission.ResponseID,
				liveMediaError(err, key),
			)
		}
		if submission.State != media.SubmissionAccepted || submission.Job == nil {
			t.Fatalf("Omni did not return a Files job: %+v", submission)
		}
		job = *submission.Job
	}
	t.Logf("polling Omni Files job: %s", job.ID)
	for {
		poll, err := client.PollVideo(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		switch poll.State {
		case media.JobPending:
			select {
			case <-ctx.Done():
				t.Fatalf("Omni Files job still pending: %v", ctx.Err())
			case <-time.After(5 * time.Second):
			}
		case media.JobSucceeded:
			result, err := client.FetchVideo(ctx, poll)
			if err != nil {
				t.Fatal(err)
			}
			checkLiveVideo(t, result, modelName, InteractionsAPI, "omni-uri")
			return
		default:
			t.Fatalf("Omni Files job ended in %s: %s", poll.State, poll.Failure)
		}
	}
}

func liveMediaError(err error, key string) string {
	providerErr, ok := errors.AsType[*media.Error](err)
	if !ok {
		return strings.ReplaceAll(err.Error(), key, "[REDACTED]")
	}
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(
		providerErr.Body,
		&response,
	) != nil || response.Error.Message == "" {
		return strings.ReplaceAll(err.Error(), key, "[REDACTED]")
	}
	return strings.ReplaceAll(
		fmt.Sprintf("%v: %s", err, response.Error.Message),
		key,
		"[REDACTED]",
	)
}

func checkLiveVideo(
	t *testing.T,
	result media.Result,
	modelName, api, name string,
) {
	t.Helper()
	if err := result.Validate(); err != nil ||
		result.Requested.Model != modelName ||
		result.API != api {
		t.Fatalf(
			"incomplete video result: validation=%v requested=%q api=%q",
			err,
			result.Requested.Model,
			result.API,
		)
	}
	for _, block := range result.Content {
		if block.Kind != content.KindVideo {
			continue
		}
		source := block.Video.Source
		if source.MIMEType != "video/mp4" ||
			len(source.Data) < 12 ||
			string(source.Data[4:8]) != "ftyp" {
			t.Fatalf(
				"invalid MP4 output: mime=%q bytes=%d",
				source.MIMEType,
				len(source.Data),
			)
		}
		if dir := os.Getenv("KERNEL_LIVE_VIDEO_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name+".mp4")
			if err := os.WriteFile(path, source.Data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("saved video: %s", path)
		}
		t.Logf("generated MP4: %d bytes", len(source.Data))
		return
	}
	t.Fatal("video result has no video block")
}
