package imagine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

func TestImageCallUsesPreparedJSONAndReturnsURL(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/generations" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf(
				"unexpected image request: %s %s %v",
				r.Method,
				r.URL.Path,
				r.Header,
			)
		}
		_, _ = io.WriteString(
			w,
			`{"data":[{"url":"https://imgen.x.ai/temporary.jpeg","mime_type":"image/jpeg"}],"usage":{"cost_in_usd_ticks":200000000}}`,
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
		Prompt:    []content.Block{content.TextBlock("A quiet lake")},
	}
	request, err := client.PrepareImage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("preparation made a network request")
	}
	result, err := client.SendImage(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("image call dispatched %d times", calls.Load())
	}
	if result.Content[0].Image.Source.URL != "https://imgen.x.ai/temporary.jpeg" {
		t.Fatalf("image URL = %q", result.Content[0].Image.Source.URL)
	}
	if result.Requested.Provider != "test-grok" {
		t.Fatalf("requested provider = %q", result.Requested.Provider)
	}
	if string(result.RawUsage) != `{"cost_in_usd_ticks":200000000}` {
		t.Fatalf("image usage was lost: %s", result.RawUsage)
	}
}

func TestVideoJobSeparatesSubmitPollAndFetch(t *testing.T) {
	var calls atomic.Int32
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
			_, _ = io.WriteString(w, `{"request_id":"request-123"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/request-123":
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"status":"pending"}`)
			} else {
				_, _ = io.WriteString(
					w,
					`{"status":"done","model":"grok-imagine-video-1.5","video":{"url":"https://vidgen.x.ai/temporary.mp4","duration":8},"usage":{"cost_in_usd_ticks":500000000}}`,
				)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	input := media.VideoInput{
		Model:    "grok-imagine-video-1.5",
		Prompt:   []content.Block{content.TextBlock("A quiet lake at sunrise")},
		Settings: media.VideoSettings{Duration: 8 * time.Second},
	}
	request, err := client.PrepareVideo(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("preparation made a network request")
	}
	submission, err := client.SubmitVideo(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if submission.State != media.SubmissionAccepted || submission.Job == nil {
		t.Fatalf("submission = %+v", submission)
	}
	pending, err := client.PollVideo(t.Context(), *submission.Job)
	if err != nil || pending.State != media.JobPending {
		t.Fatalf("pending poll = %+v, %v", pending, err)
	}
	done, err := client.PollVideo(t.Context(), *submission.Job)
	if err != nil || done.State != media.JobSucceeded {
		t.Fatalf("completed poll = %+v, %v", done, err)
	}
	result, err := client.FetchVideo(t.Context(), done)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("job lifecycle dispatched %d times", calls.Load())
	}
	if result.Content[0].Video.Source.URL != "https://vidgen.x.ai/temporary.mp4" {
		t.Fatalf("video URL = %q", result.Content[0].Video.Source.URL)
	}
	if result.Served.Model != "grok-imagine-video-1.5" {
		t.Fatalf("served model = %q", result.Served.Model)
	}
	if result.Content[0].Video.Duration != 8*time.Second {
		t.Fatalf("reported duration = %v", result.Content[0].Video.Duration)
	}
	if string(done.RawUsage) != `{"cost_in_usd_ticks":500000000}` ||
		string(result.RawUsage) != string(done.RawUsage) {
		t.Fatalf(
			"video usage was lost: poll=%s result=%s",
			done.RawUsage,
			result.RawUsage,
		)
	}
}

func TestVideoSubmitOutcomeStaysUnknownAfterTransportFailure(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("connection dropped after request write")
	})
	client := testClient(
		t,
		"https://example.test/v1",
		&http.Client{Transport: transport},
		model.CapabilityAdjust,
	)
	request, err := client.PrepareVideo(
		t.Context(),
		media.VideoInput{
			Model:  "unlisted-video-model",
			Prompt: []content.Block{content.TextBlock("Waves")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := client.SubmitVideo(t.Context(), request)
	if err == nil ||
		submission.State != media.SubmissionUnknown ||
		submission.Job != nil {
		t.Fatalf("submission = %+v, err = %v", submission, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("transport dispatched %d attempts", calls.Load())
	}
}

func TestVideoSubmitHTTPOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status int
		state  media.SubmissionState
	}{
		{status: http.StatusBadRequest, state: media.SubmissionRejected},
		{status: http.StatusTooManyRequests, state: media.SubmissionUnknown},
		{status: http.StatusInternalServerError, state: media.SubmissionUnknown},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"code":"failure"}}`)
			}))
			defer server.Close()
			client := testClient(
				t,
				server.URL+"/v1",
				server.Client(),
				model.CapabilityStrict,
			)
			request, err := client.PrepareVideo(
				t.Context(),
				media.VideoInput{
					Model:  "grok-imagine-video-1.5",
					Prompt: []content.Block{content.TextBlock("Waves")},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			submission, err := client.SubmitVideo(t.Context(), request)
			if err == nil || submission.State != tc.state || submission.Job != nil {
				t.Fatalf("submission = %+v, err = %v", submission, err)
			}
		})
	}
}

func TestVideoSubmitWithoutUsableHandleStaysUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"request_id":"  "}`)
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	request, err := client.PrepareVideo(
		t.Context(),
		media.VideoInput{
			Model:  "grok-imagine-video-1.5",
			Prompt: []content.Block{content.TextBlock("Waves")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := client.SubmitVideo(t.Context(), request)
	if err == nil ||
		submission.State != media.SubmissionUnknown ||
		submission.Job != nil {
		t.Fatalf("submission = %+v, err = %v", submission, err)
	}
}

func TestVideoPollTreatsModerationFilterAsFailedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(
			w,
			`{"status":"done","video":{"respect_moderation":false}}`,
		)
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	job := media.Job{
		ID:     "filtered-request",
		Kind:   media.KindVideo,
		Origin: client.origin(VideoAPI, "grok-imagine-video-1.5"),
	}
	poll, err := client.PollVideo(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if poll.State != media.JobFailed ||
		poll.Failure == "" ||
		len(poll.Content) != 0 {
		t.Fatalf("moderated poll = %+v", poll)
	}
}

func TestPreparedImageCannotCrossCredentialScope(t *testing.T) {
	clientA := testClient(
		t,
		"https://example.test/v1",
		nil,
		model.CapabilityAdjust,
	)
	clientB, err := New(Config{
		Provider:         "test-grok",
		APIKey:           "other-key",
		Endpoint:         "https://example.test/v1",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := clientA.PrepareImage(
		t.Context(),
		media.ImageInput{
			Model:     "unlisted-image-model",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("A lake")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientB.SendImage(t.Context(), request); err == nil {
		t.Fatal("prepared request crossed credential scope")
	}
}

func TestNewCustomEndpointRequiresProviderIdentity(t *testing.T) {
	if _, err := New(Config{
		APIKey:   "test-key",
		Endpoint: "https://example.test/v1",
	}); err == nil {
		t.Fatal("custom endpoint inherited the public Grok identity")
	}
	client, err := New(Config{
		Provider:         "private-grok",
		APIKey:           "test-key",
		Endpoint:         "https://example.test/v1",
		CapabilityPolicy: model.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.PrepareImage(
		t.Context(),
		media.ImageInput{
			Model:     "unlisted-image-model",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("A lake")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.Origin().Provider != "private-grok" {
		t.Fatalf("prepared provider = %q", request.Origin().Provider)
	}
}

func testClient(
	t *testing.T,
	endpoint string,
	httpClient *http.Client,
	unknown model.CapabilityPolicy,
) *Client {
	t.Helper()
	models := map[string]media.ModelInfo{}
	for _, entry := range []struct {
		name string
		api  string
	}{
		{name: "grok-imagine-image-2.0", api: ImageAPI},
		{name: "grok-imagine-video-1.5", api: VideoAPI},
	} {
		info := media.ModelInfo{
			Evidence: model.ModelEvidence{
				Model:       entry.name,
				API:         entry.api,
				Endpoint:    endpoint,
				Owner:       "test",
				Source:      "fixture",
				Method:      model.EvidenceFixture,
				QualifiedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
			},
		}
		if entry.api == ImageAPI {
			info.Image = &media.ImageCapabilities{
				Generate: model.SupportSupported,
				Edit:     model.SupportSupported,
				MaxCount: 10,
			}
		} else {
			info.Video = &media.VideoCapabilities{
				Generate:        model.SupportSupported,
				ReferenceFrames: model.SupportSupported,
				MinDuration:     time.Second,
				MaxDuration:     15 * time.Second,
			}
		}
		models[entry.name] = info
	}
	client, err := New(Config{
		Provider:         "test-grok",
		APIKey:           "test-key",
		Endpoint:         endpoint,
		Client:           httpClient,
		Models:           models,
		CapabilityPolicy: unknown,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestVideoPollAcceptsNullUsageAndRejectsDotSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/videos/request-123" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"status":"pending","usage":null}`)
	}))
	defer server.Close()
	client := testClient(t, server.URL+"/v1", server.Client(), model.CapabilityStrict)
	job := media.Job{ID: "request-123", Kind: media.KindVideo, Origin: client.origin(VideoAPI, "grok-imagine-video-1.5")}
	poll, err := client.PollVideo(t.Context(), job)
	if err != nil || poll.State != media.JobPending || poll.RawUsage != nil {
		t.Fatalf("poll %+v, err %v", poll, err)
	}
	for _, id := range []string{".", ".."} {
		job.ID = id
		if _, err := client.PollVideo(t.Context(), job); err == nil {
			t.Fatalf("job ID %q accepted", id)
		}
	}
}

func TestVideoSubmitBeforeDispatchIsRejected(t *testing.T) {
	client := testClient(t, "https://api.x.ai/v1", nil, model.CapabilityStrict)
	request, err := client.PrepareVideo(t.Context(), media.VideoInput{
		Model:  "grok-imagine-video-1.5",
		Prompt: []content.Block{content.TextBlock("A quiet lake at sunrise")},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if sub, err := client.SubmitVideo(canceled, request); err == nil || sub.State != media.SubmissionRejected || sub.Validate() != nil {
		t.Fatalf("canceled submission %+v, err %v", sub, err)
	}
	client.noKey = errors.New("no key")
	if sub, err := client.SubmitVideo(t.Context(), request); err == nil || sub.State != media.SubmissionRejected {
		t.Fatalf("keyless submission %+v, err %v", sub, err)
	}
}

func TestPrepareRejectsModelOfTheOtherKind(t *testing.T) {
	client := testClient(t, "https://api.x.ai/v1", nil, model.CapabilityAdjust)
	_, err := client.PrepareImage(t.Context(), media.ImageInput{
		Operation: media.ImageGenerate,
		Model:     "grok-imagine-video-1.5",
		Prompt:    []content.Block{content.TextBlock("A lake")},
	})
	if e, ok := errors.AsType[*media.Error](err); !ok || e.Category != media.ErrorUnsupported {
		t.Fatalf("video model prepared as image: %v", err)
	}
	_, err = client.PrepareVideo(t.Context(), media.VideoInput{
		Operation: media.VideoGenerate,
		Model:     "grok-imagine-image-2.0",
		Prompt:    []content.Block{content.TextBlock("A lake")},
	})
	if e, ok := errors.AsType[*media.Error](err); !ok || e.Category != media.ErrorUnsupported {
		t.Fatalf("image model prepared as video: %v", err)
	}
}
