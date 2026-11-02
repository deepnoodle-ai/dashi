package gemini_test

import (
	"context"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/providers/google/gemini"
)

func ExampleNewVertexVideoJobs() {
	// Credentials come from Application Default Credentials on the first
	// send; construction and preparation need none.
	jobs, err := gemini.NewVertexVideoJobs(gemini.VertexVideoConfig{
		Project: "my-project", // or GOOGLE_CLOUD_PROJECT
	})
	if err != nil {
		panic(err)
	}
	info, _ := jobs.VideoModelInfo("veo-3.1-fast-generate-001")
	fmt.Println(jobs.Provider(), info.Video.Resolutions)

	request, err := jobs.PrepareVideo(context.Background(), media.VideoInput{
		Model:  "veo-3.1-fast-generate-001",
		Prompt: []content.Block{content.TextBlock("A cup turns on a table")},
		Settings: media.VideoSettings{
			Duration:   8 * time.Second,
			Resolution: "1080p",
		},
	})
	if err != nil {
		panic(err)
	}
	// Persist the request, then SubmitVideo once and persist the Job it
	// returns. Poll with PollVideo until the job is done, then FetchVideo
	// returns the MP4 bytes and Result.VideoUsage for billing.
	fmt.Println(string(request.Body()))
	// Output:
	// google-vertex [720p 1080p 4k]
	// {"instances":[{"prompt":"A cup turns on a table"}],"parameters":{"durationSeconds":8,"generateAudio":true,"resolution":"1080p","sampleCount":1}}
}
