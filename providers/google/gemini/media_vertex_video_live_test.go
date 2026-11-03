//go:build live

package gemini

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

// Run with DASHI_LIVE_VERTEX_VIDEO_TEST=1, GOOGLE_CLOUD_PROJECT, and ADC
// (gcloud auth application-default login); GOOGLE_CLOUD_LOCATION is optional.
// This makes one paid Veo 3.1 Fast submission, 8 s at 1080p with audio
// (about $0.96), then polls and measures that job. DASHI_LIVE_VERTEX_VEO_JOB_ID
// resumes an existing job without submitting another. DASHI_LIVE_VIDEO_DIR
// saves the video. DASHI_LIVE_VERTEX_ACCESS_TOKEN (for example from gcloud
// auth print-access-token) replaces ADC with a supplied Client.
func TestLiveVertexVeoVideoJob(t *testing.T) {
	if os.Getenv("DASHI_LIVE_VERTEX_VIDEO_TEST") != "1" {
		t.Skip("set DASHI_LIVE_VERTEX_VIDEO_TEST=1 to make a paid Vertex Veo call")
	}
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" {
		t.Fatal("GOOGLE_CLOUD_PROJECT is required")
	}
	const modelName = "veo-3.1-fast-generate-001"
	var config VertexVideoConfig
	if token := os.Getenv("DASHI_LIVE_VERTEX_ACCESS_TOKEN"); token != "" {
		config.Client = &http.Client{Transport: bearerTransport{
			token:   token,
			project: os.Getenv("GOOGLE_CLOUD_PROJECT"),
		}}
	}
	client, err := NewVertexVideoJobs(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("endpoint: %s", client.videoURL(modelName))
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	job := media.Job{
		ID:     os.Getenv("DASHI_LIVE_VERTEX_VEO_JOB_ID"),
		Kind:   media.KindVideo,
		Origin: client.origin(modelName),
	}
	if job.ID == "" {
		request, err := client.PrepareVideo(ctx, media.VideoInput{
			Model: modelName,
			Prompt: []content.Block{
				content.TextBlock("A single blue ceramic cup slowly rotates on a plain white table while a soft piano plays. Steady camera, no people, no text."),
			},
			Settings: media.VideoSettings{
				Duration:    8 * time.Second,
				Resolution:  "1080p",
				AspectRatio: "16:9",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("request body: %s", request.Body())
		submission, err := client.SubmitVideo(ctx, request)
		if err != nil {
			t.Fatalf("submit (state %s): %v", submission.State, err)
		}
		job = *submission.Job
	}
	// The name shows whether Vertex uses the project ID or number.
	t.Logf("operation name: %s", job.ID)
	for {
		poll, err := client.PollVideo(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		switch poll.State {
		case media.JobPending:
			select {
			case <-ctx.Done():
				t.Fatalf("Vertex Veo job still pending: %v", ctx.Err())
			case <-time.After(10 * time.Second):
			}
			continue
		case media.JobSucceeded:
		default:
			t.Fatalf("Vertex Veo job ended in %s: %s", poll.State, poll.Failure)
		}
		result, err := client.FetchVideo(ctx, poll)
		if err != nil {
			t.Fatal(err)
		}
		checkLiveVideo(t, result, modelName, VideoAPI, "vertex-veo")
		if len(result.VideoUsage) != 1 {
			t.Fatalf("video was not measured: %+v", result.VideoUsage)
		}
		usage := result.VideoUsage[0]
		t.Logf("usage: %+v", usage)
		frame := time.Second / 24
		if d := usage.Duration - 8*time.Second; d > frame || d < -frame ||
			usage.Width != 1920 || usage.Height != 1080 ||
			usage.Resolution != "1080p" || !usage.Audio {
			t.Fatalf("usage does not describe an 8 s 1080p video with audio: %+v", usage)
		}
		return
	}
}

// bearerTransport adds a fixed access token and quota project, as a host's
// own credential transport would.
type bearerTransport struct{ token, project string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set("X-Goog-User-Project", b.project)
	return http.DefaultTransport.RoundTrip(r)
}
