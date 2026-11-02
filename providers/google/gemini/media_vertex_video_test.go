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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/mp4/mp4test"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/model"
)

const vertexTestPath = "/v1/projects/proj/locations/us-central1/publishers/google/models/"

func clearVertexEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION",
		"GOOGLE_CLOUD_REGION",
	} {
		t.Setenv(name, "")
	}
}

func newVertexFixture(t *testing.T, server *httptest.Server) *VertexVideoJobClient {
	t.Helper()
	p, err := NewVertexVideoJobs(VertexVideoConfig{
		Provider: "google-vertex-test",
		Project:  "proj",
		Location: "us-central1",
		Endpoint: server.URL + vertexTestPath + "{model}:predictLongRunning",
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func vertexInput(settings media.VideoSettings) media.VideoInput {
	return media.VideoInput{
		Model:    "veo-3.1-fast-generate-001",
		Prompt:   []content.Block{content.TextBlock("A cup turns on a table")},
		Settings: settings,
	}
}

func TestVertexVideoConstruction(t *testing.T) {
	clearVertexEnv(t)
	if _, err := NewVertexVideoJobs(VertexVideoConfig{}); err == nil ||
		!strings.Contains(err.Error(), "set Project or GOOGLE_CLOUD_PROJECT") {
		t.Fatalf("missing project error = %v", err)
	}
	for _, tc := range []struct {
		name     string
		config   VertexVideoConfig
		env      map[string]string
		endpoint string
	}{
		{
			"defaults",
			VertexVideoConfig{Project: "proj"},
			nil,
			"https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		{
			"env project and location",
			VertexVideoConfig{},
			map[string]string{
				"GOOGLE_CLOUD_PROJECT":  "env-proj",
				"GOOGLE_CLOUD_LOCATION": "europe-west4",
			},
			"https://europe-west4-aiplatform.googleapis.com/v1/projects/env-proj/locations/europe-west4/publishers/google/models/{model}:predictLongRunning",
		},
		{
			"region fallback",
			VertexVideoConfig{Project: "proj"},
			map[string]string{"GOOGLE_CLOUD_REGION": "us-east5"},
			"https://us-east5-aiplatform.googleapis.com/v1/projects/proj/locations/us-east5/publishers/google/models/{model}:predictLongRunning",
		},
		{
			"global in config",
			VertexVideoConfig{Project: "proj", Location: "global"},
			nil,
			"https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		{
			"global in env",
			VertexVideoConfig{Project: "proj"},
			map[string]string{"GOOGLE_CLOUD_LOCATION": "global"},
			"https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		{
			"multi-region",
			VertexVideoConfig{Project: "proj", Location: "us"},
			nil,
			"https://aiplatform.us.rep.googleapis.com/v1/projects/proj/locations/us/publishers/google/models/{model}:predictLongRunning",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearVertexEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			p, err := NewVertexVideoJobs(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if p.config.Endpoint != tc.endpoint || p.Provider() != "google-vertex" ||
				!p.adc {
				t.Fatalf("config = %+v", p.config)
			}
			if p.config.CredentialScope != "project:"+p.config.Project {
				t.Fatalf("credential scope = %q", p.config.CredentialScope)
			}
		})
	}
	scoped := "https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning"
	for name, config := range map[string]VertexVideoConfig{
		"custom endpoint without provider": {
			Project:  "proj",
			Endpoint: "https://proxy.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		"endpoint for another project": {
			Provider: "p", Project: "proj",
			Endpoint: strings.Replace(scoped, "/proj/", "/other/", 1),
		},
		"endpoint for another location": {
			Provider: "p", Project: "proj", Location: "europe-west4",
			Endpoint: scoped,
		},
		"custom host without client": {
			Provider: "p", Project: "proj",
			Endpoint: "https://evil.example/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		"look-alike host without client": {
			Provider: "p", Project: "proj",
			Endpoint: "https://googleapis.com.evil.example/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		},
		"http without client": {
			Provider: "p", Project: "proj",
			Endpoint: strings.Replace(scoped, "https:", "http:", 1),
		},
		"wrong method": {
			Provider: "p", Project: "proj",
			Endpoint: strings.Replace(scoped, ":predictLongRunning", ":generateContent", 1),
		},
		"bad policy": {
			Project: "proj", CapabilityPolicy: "loose",
		},
		"model evidence for another endpoint": {
			Project: "proj",
			VideoModels: map[string]media.ModelInfo{
				"veo-x": {
					Video: &media.VideoCapabilities{Generate: model.SupportSupported},
					Evidence: model.ModelEvidence{
						Model: "veo-x", API: VideoAPI, Endpoint: "https://example.test/veo-x",
						Owner: "app", Source: "test", Method: model.EvidenceFixture,
						QualifiedAt: time.Now(),
					},
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearVertexEnv(t)
			if _, err := NewVertexVideoJobs(config); err == nil {
				t.Fatal("bad config accepted")
			}
		})
	}
	// A custom host is fine with a Client, which supplies its own
	// credentials.
	p, err := NewVertexVideoJobs(VertexVideoConfig{
		Provider: "proxy", Project: "proj",
		Endpoint: "http://127.0.0.1:9/v1/projects/proj/locations/us-central1/publishers/google/models/{model}:predictLongRunning",
		Client:   &http.Client{},
	})
	if err != nil || p.adc {
		t.Fatalf("custom host with client: %v, adc %v", err, p != nil && p.adc)
	}
}

func TestVertexVideoBuiltInModels(t *testing.T) {
	clearVertexEnv(t)
	override := media.ModelInfo{
		Video: &media.VideoCapabilities{
			Generate:    model.SupportSupported,
			Resolutions: []string{"720p"},
		},
		Evidence: model.ModelEvidence{
			Model: "veo-3.1-generate-001", API: VideoAPI,
			Endpoint: "https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/veo-3.1-generate-001:predictLongRunning",
			Owner:    "app", Source: "probe", Method: model.EvidenceLiveProbe,
			QualifiedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	p, err := NewVertexVideoJobs(VertexVideoConfig{
		Project:     "proj",
		VideoModels: map[string]media.ModelInfo{"veo-3.1-generate-001": override},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		resolutions int
		frames      model.Support
		method      model.EvidenceMethod
	}{
		"veo-3.1-generate-001":      {1, "", model.EvidenceLiveProbe},
		"veo-3.1-fast-generate-001": {3, model.SupportSupported, model.EvidenceInference},
		"veo-3.1-lite-generate-001": {2, model.SupportUnknown, model.EvidenceDocumentation},
	} {
		info, ok := p.VideoModelInfo(name)
		if !ok || info.Validate() != nil {
			t.Fatalf("%s: missing or invalid info", name)
		}
		if len(info.Video.Resolutions) != want.resolutions ||
			info.Video.ReferenceFrames != want.frames ||
			info.Evidence.Method != want.method ||
			info.Evidence.Endpoint != p.videoURL(name) {
			t.Fatalf("%s: info = %+v %+v", name, info.Video, info.Evidence)
		}
	}
	fast, _ := p.VideoModelInfo("veo-3.1-fast-generate-001")
	if !strings.Contains(fast.Evidence.Source, "untested") {
		t.Fatalf("Fast 4k is not marked untested: %q", fast.Evidence.Source)
	}
	fast.Video.Resolutions[0] = "changed"
	if again, _ := p.VideoModelInfo("veo-3.1-fast-generate-001"); again.Video.Resolutions[0] != "720p" {
		t.Fatal("model info snapshot was shared")
	}
}

func TestVertexVideoRequestBody(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newVertexFixture(t, server)
	frame := content.ImageBlock(content.Source{Data: []byte("png"), MIMEType: "image/png"})
	for _, tc := range []struct {
		name  string
		input media.VideoInput
		want  string
	}{
		{
			"text",
			vertexInput(media.VideoSettings{
				Duration: 8 * time.Second, Resolution: "1080p", AspectRatio: "16:9",
			}),
			`{"instances":[{"prompt":"A cup turns on a table"}],"parameters":{"aspectRatio":"16:9","durationSeconds":8,"generateAudio":true,"resolution":"1080p","sampleCount":1}}`,
		},
		{
			"defaults",
			vertexInput(media.VideoSettings{}),
			`{"instances":[{"prompt":"A cup turns on a table"}],"parameters":{"generateAudio":true,"sampleCount":1}}`,
		},
		{
			"audio off",
			vertexInput(media.VideoSettings{DisableAudio: true}),
			`{"instances":[{"prompt":"A cup turns on a table"}],"parameters":{"generateAudio":false,"sampleCount":1}}`,
		},
		{
			"first frame",
			media.VideoInput{
				Model:           "veo-3.1-generate-001",
				ReferenceFrames: []content.Block{frame},
			},
			`{"instances":[{"image":{"bytesBase64Encoded":"cG5n","mimeType":"image/png"}}],"parameters":{"generateAudio":true,"sampleCount":1}}`,
		},
		{
			// Dashi leaves the Gemini API's 8 s rule for high resolution
			// to Vertex.
			"4 s at 1080p",
			vertexInput(media.VideoSettings{Duration: 4 * time.Second, Resolution: "1080p"}),
			`{"instances":[{"prompt":"A cup turns on a table"}],"parameters":{"durationSeconds":4,"generateAudio":true,"resolution":"1080p","sampleCount":1}}`,
		},
		{
			"Veo 2 omits audio",
			media.VideoInput{
				Model:  "veo-2.0-generate-001",
				Prompt: []content.Block{content.TextBlock("A cup")},
			},
			`{"instances":[{"prompt":"A cup"}],"parameters":{"sampleCount":1}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := p.PrepareVideo(t.Context(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if string(request.Body()) != tc.want {
				t.Fatalf("body:\n got %s\nwant %s", request.Body(), tc.want)
			}
		})
	}
	unknown, err := p.PrepareVideo(t.Context(), media.VideoInput{
		Model:  "veo-4.0-generate-001",
		Prompt: []content.Block{content.TextBlock("A cup")},
	})
	if err != nil || len(unknown.Snapshot().Unverified) == 0 {
		t.Fatalf("unknown model under adjust: %v %v", err, unknown.Snapshot().Unverified)
	}
}

func TestVertexVideoPrepareRejects(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newVertexFixture(t, server)
	png := content.ImageBlock(content.Source{Data: []byte("png"), MIMEType: "image/png"})
	for _, tc := range []struct {
		name  string
		input media.VideoInput
		names string
	}{
		{"edit", func() media.VideoInput {
			in := vertexInput(media.VideoSettings{})
			in.Operation = media.VideoEdit
			return in
		}(), "edit"},
		{"exact size", vertexInput(media.VideoSettings{
			Size: media.Size{Width: 640, Height: 480},
		}), "640x480"},
		{"two frames", func() media.VideoInput {
			in := vertexInput(media.VideoSettings{})
			in.ReferenceFrames = []content.Block{png, png}
			return in
		}(), "2"},
		{"square", vertexInput(media.VideoSettings{AspectRatio: "1:1"}), `"1:1"`},
		{"5 s", vertexInput(media.VideoSettings{Duration: 5 * time.Second}), "5s"},
		{"480p", vertexInput(media.VideoSettings{Resolution: "480p"}), `"480p"`},
		{"Lite 4k", media.VideoInput{
			Model:    "veo-3.1-lite-generate-001",
			Prompt:   []content.Block{content.TextBlock("A cup")},
			Settings: media.VideoSettings{Resolution: "4k"},
		}, `"4k"`},
		{"URL frame", func() media.VideoInput {
			in := vertexInput(media.VideoSettings{})
			in.ReferenceFrames = []content.Block{
				content.ImageBlock(content.Source{URL: "https://example.test/f.png"}),
			}
			return in
		}(), "inline"},
		{"GIF frame", func() media.VideoInput {
			in := vertexInput(media.VideoSettings{})
			in.ReferenceFrames = []content.Block{
				content.ImageBlock(content.Source{Data: []byte("gif"), MIMEType: "image/gif"}),
			}
			return in
		}(), "PNG or JPEG"},
		{"empty prompt", media.VideoInput{
			Model:  "veo-3.1-fast-generate-001",
			Prompt: []content.Block{content.TextBlock("  ")},
		}, "nonempty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.PrepareVideo(t.Context(), tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("error = %v, want one naming %s", err, tc.names)
			}
		})
	}
}

// vertexServer answers predictLongRunning with submit and
// fetchPredictOperation with the next poll body.
type vertexServer struct {
	t        *testing.T
	submit   string
	polls    []string
	requests atomic.Int32
}

func (s *vertexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		s.t.Errorf("request %s %s", r.Method, r.Header.Get("Content-Type"))
	}
	switch {
	case strings.HasSuffix(r.URL.Path, ":predictLongRunning"):
		io.WriteString(w, s.submit)
	case strings.HasSuffix(r.URL.Path, ":fetchPredictOperation"):
		if r.URL.Path != vertexTestPath+"veo-3.1-fast-generate-001:fetchPredictOperation" {
			s.t.Errorf("poll path %s", r.URL.Path)
		}
		var poll struct {
			OperationName string `json:"operationName"`
		}
		if json.Unmarshal(body, &poll) != nil || !strings.HasSuffix(poll.OperationName, "/operations/op-1") {
			s.t.Errorf("poll body %s", body)
		}
		next := s.polls[0]
		if len(s.polls) > 1 {
			s.polls = s.polls[1:]
		}
		io.WriteString(w, next)
	default:
		s.t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func vertexOperation(project string) string {
	return "projects/" + project +
		"/locations/us-central1/publishers/google/models/veo-3.1-fast-generate-001/operations/op-1"
}

func doneWithVideo(name string, video []byte) string {
	return fmt.Sprintf(
		`{"name":%q,"done":true,"response":{"@type":"type.googleapis.com/cloud.ai.large_models.vision.GenerateVideoResponse","raiMediaFilteredCount":0,"videos":[{"bytesBase64Encoded":%q,"mimeType":"video/mp4"}]}}`,
		name,
		base64.StdEncoding.EncodeToString(video),
	)
}

func TestVertexVideoJobLifecycle(t *testing.T) {
	video := mp4test.Build(mp4test.VideoAudio(8, 43, 1920, 1080))
	// Vertex may name the project by number in the operation.
	name := vertexOperation("123456789012")
	handler := &vertexServer{
		t:      t,
		submit: fmt.Sprintf(`{"name":%q}`, name),
		polls:  []string{fmt.Sprintf(`{"name":%q}`, name), doneWithVideo(name, video)},
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	p := newVertexFixture(t, server)
	request, err := p.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{
		Duration: 8 * time.Second, Resolution: "1080p",
	}))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := p.SubmitVideo(t.Context(), request)
	if err != nil || submission.State != media.SubmissionAccepted ||
		submission.Job.ID != name || submission.Validate() != nil {
		t.Fatalf("submit = %+v, %v", submission, err)
	}
	poll, err := p.PollVideo(t.Context(), *submission.Job)
	if err != nil || poll.State != media.JobPending {
		t.Fatalf("first poll = %+v, %v", poll, err)
	}
	poll, err = p.PollVideo(t.Context(), *submission.Job)
	if err != nil || poll.State != media.JobSucceeded || poll.Validate() != nil {
		t.Fatalf("second poll = %+v, %v", poll, err)
	}
	sent := handler.requests.Load()
	result, err := p.FetchVideo(t.Context(), poll)
	if err != nil {
		t.Fatal(err)
	}
	if handler.requests.Load() != sent {
		t.Fatal("FetchVideo sent a request")
	}
	if len(result.Content) != 1 ||
		string(result.Content[0].Video.Source.Data) != string(video) ||
		result.Content[0].Video.Source.MIMEType != "video/mp4" ||
		result.Content[0].Video.Duration != 8*time.Second ||
		result.ResponseID != name || result.API != VideoAPI ||
		result.Requested.Model != "veo-3.1-fast-generate-001" {
		t.Fatalf("result = %+v", result)
	}
	want := media.VideoUsage{
		ContentIndex: 0, Duration: 8 * time.Second,
		Width: 1920, Height: 1080, Resolution: "1080p", Audio: true,
	}
	if len(result.VideoUsage) != 1 || result.VideoUsage[0] != want {
		t.Fatalf("usage = %+v", result.VideoUsage)
	}
	poll.Content[0].Video.Source.Data[0] ^= 0xff
	if result.Content[0].Video.Source.Data[0] == poll.Content[0].Video.Source.Data[0] {
		t.Fatal("result shares bytes with the poll")
	}
}

func TestVertexVideoOperationNames(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newVertexFixture(t, server)
	const m = "veo-3.1-fast-generate-001"
	for _, name := range []string{
		vertexOperation("proj"),
		vertexOperation("123456789012"),
		"projects/proj/locations/us-central1/publishers/google/models/" + m + "/operations/a1_B-2",
	} {
		if err := p.checkOperation(m, name); err != nil {
			t.Errorf("rejected %s: %v", name, err)
		}
	}
	for _, name := range []string{
		"",
		strings.Replace(vertexOperation("proj"), "us-central1", "europe-west4", 1),
		strings.Replace(vertexOperation("proj"), m, "veo-3.1-generate-001", 1),
		vertexOperation("proj") + "/extra",
		"/" + vertexOperation("proj"),
		vertexOperation("proj/../other"),
		strings.Replace(vertexOperation("proj"), "op-1", "op%2F1", 1),
		strings.Replace(vertexOperation("proj"), "op-1", "op-1?x=1", 1),
		strings.Replace(vertexOperation("proj"), "publishers/google", "publishers/evil", 1),
		"models/" + m + "/operations/op-1",
		"operations/op-1",
	} {
		if err := p.checkOperation(m, name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	// A bad name from Vertex leaves the submission unknown, never accepted.
	handler := &vertexServer{t: t, submit: `{"name":"operations/op-1"}`}
	server2 := httptest.NewServer(handler)
	defer server2.Close()
	p2 := newVertexFixture(t, server2)
	request, _ := p2.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
	submission, err := p2.SubmitVideo(t.Context(), request)
	if err == nil || submission.State != media.SubmissionUnknown || submission.Job != nil {
		t.Fatalf("bad name submission = %+v, %v", submission, err)
	}
	// A forged job ID is refused before anything is sent.
	job := media.Job{ID: "operations/op-1", Kind: media.KindVideo, Origin: p2.origin(m)}
	sent := handler.requests.Load()
	if _, err := p2.PollVideo(t.Context(), job); err == nil || handler.requests.Load() != sent {
		t.Fatalf("forged job polled: %v", err)
	}
	// A job from another destination is refused.
	job = media.Job{ID: vertexOperation("proj"), Kind: media.KindVideo, Origin: p.origin(m)}
	if _, err := p2.PollVideo(t.Context(), job); err == nil {
		t.Fatal("foreign job polled")
	}
}

func TestVertexVideoSubmitStatus(t *testing.T) {
	for _, tc := range []struct {
		status   int
		state    media.SubmissionState
		category media.ErrorCategory
	}{
		{400, media.SubmissionRejected, media.ErrorInvalidRequest},
		{401, media.SubmissionRejected, media.ErrorAuthentication},
		{403, media.SubmissionRejected, media.ErrorAuthentication},
		{404, media.SubmissionRejected, media.ErrorInvalidRequest},
		{408, media.SubmissionUnknown, media.ErrorInvalidRequest},
		{429, media.SubmissionUnknown, media.ErrorRateLimited},
		{503, media.SubmissionUnknown, media.ErrorOverloaded},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			p := newVertexFixture(t, server)
			request, _ := p.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
			submission, err := p.SubmitVideo(t.Context(), request)
			providerErr, ok := errors.AsType[*media.Error](err)
			if submission.State != tc.state || !ok || providerErr.Category != tc.category {
				t.Fatalf("submit = %+v, %v", submission, err)
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	p := newVertexFixture(t, server)
	server.Close()
	request, _ := p.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
	if submission, err := p.SubmitVideo(t.Context(), request); err == nil ||
		submission.State != media.SubmissionUnknown {
		t.Fatalf("transport failure = %+v, %v", submission, err)
	}
}

func TestVertexVideoPollOutcomes(t *testing.T) {
	name := vertexOperation("proj")
	for _, tc := range []struct {
		label   string
		body    string
		state   media.JobState
		failure string
		err     string
	}{
		{"pending", `{"name":"` + name + `","done":false}`, media.JobPending, "", ""},
		{"error message", `{"name":"` + name + `","done":true,"error":{"code":3,"message":"bad prompt"}}`,
			media.JobFailed, "bad prompt", ""},
		{"error code", `{"name":"` + name + `","done":true,"error":{"code":13}}`,
			media.JobFailed, "code 13", ""},
		{"safety filter", `{"name":"` + name + `","done":true,"response":{"raiMediaFilteredCount":1,"raiMediaFilteredReasons":["contains a person"]}}`,
			media.JobFailed, "blocked by safety filters: contains a person", ""},
		{"Cloud Storage only", `{"name":"` + name + `","done":true,"response":{"videos":[{"gcsUri":"gs://b/v.mp4","mimeType":"video/mp4"}]}}`,
			"", "", "no inline bytes"},
		{"no videos", `{"name":"` + name + `","done":true,"response":{}}`, "", "", "no generated video"},
		{"other name", strings.Replace(doneWithVideo(name, []byte("v")), "op-1", "op-2", 1), "", "", "identity"},
		{"not video", `{"name":"` + name + `","done":true,"response":{"videos":[{"bytesBase64Encoded":"dg==","mimeType":"image/png"}]}}`,
			"", "", "non-video"},
		{"undecodable", `{"name":`, "", "", "decode"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			handler := &vertexServer{t: t, polls: []string{tc.body}}
			server := httptest.NewServer(handler)
			defer server.Close()
			p := newVertexFixture(t, server)
			job := media.Job{ID: name, Kind: media.KindVideo, Origin: p.origin("veo-3.1-fast-generate-001")}
			poll, err := p.PollVideo(t.Context(), job)
			if tc.err != "" {
				providerErr, ok := errors.AsType[*media.Error](err)
				if !ok || providerErr.Category != media.ErrorProtocol ||
					!strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want protocol error %q", err, tc.err)
				}
				return
			}
			if err != nil || poll.State != tc.state ||
				!strings.Contains(poll.Failure, tc.failure) || poll.Validate() != nil {
				t.Fatalf("poll = %+v, %v", poll, err)
			}
		})
	}
	// A missing MIME type means MP4.
	handler := &vertexServer{t: t, polls: []string{
		`{"name":"` + name + `","done":true,"response":{"videos":[{"bytesBase64Encoded":"dg=="}]}}`,
	}}
	server := httptest.NewServer(handler)
	defer server.Close()
	p := newVertexFixture(t, server)
	job := media.Job{ID: name, Kind: media.KindVideo, Origin: p.origin("veo-3.1-fast-generate-001")}
	poll, err := p.PollVideo(t.Context(), job)
	if err != nil || poll.Content[0].Video.Source.MIMEType != "video/mp4" {
		t.Fatalf("poll = %+v, %v", poll, err)
	}
}

func TestVertexVideoFetchUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("FetchVideo sent a request")
	}))
	defer server.Close()
	p := newVertexFixture(t, server)
	withEdits := mp4test.VideoAudio(8, 43, 1920, 1080)
	withEdits.Tracks[0].Duration = 8*24 + 2
	withEdits.Tracks[0].Edits = []mp4test.Edit{{Segment: 8000, MediaTime: 2}}
	withEdits.Tracks[1].Edits = []mp4test.Edit{{Segment: 8043, MediaTime: 1024}}
	silent720 := mp4test.File{
		MovieTimescale: 1000,
		Tracks: []mp4test.Track{{
			Handler: "vide", Timescale: 24, Duration: 96, Width: 1280, Height: 720,
		}},
	}
	for _, tc := range []struct {
		name string
		data []byte
		want *media.VideoUsage
	}{
		{"720p silent", mp4test.Build(silent720),
			&media.VideoUsage{Duration: 4 * time.Second, Width: 1280, Height: 720, Resolution: "720p"}},
		{"1080p with audio", mp4test.Build(mp4test.VideoAudio(8, 0, 1920, 1080)),
			&media.VideoUsage{Duration: 8 * time.Second, Width: 1920, Height: 1080, Resolution: "1080p", Audio: true}},
		{"9:16 4k", mp4test.Build(mp4test.VideoAudio(6, 0, 2160, 3840)),
			&media.VideoUsage{Duration: 6 * time.Second, Width: 2160, Height: 3840, Resolution: "4k", Audio: true}},
		{"long audio, no edit list", mp4test.Build(mp4test.VideoAudio(8, 43, 1920, 1080)),
			&media.VideoUsage{Duration: 8 * time.Second, Width: 1920, Height: 1080, Resolution: "1080p", Audio: true}},
		{"long audio, edit list", mp4test.Build(withEdits),
			&media.VideoUsage{Duration: 8 * time.Second, Width: 1920, Height: 1080, Resolution: "1080p", Audio: true}},
		{"odd size", mp4test.Build(mp4test.VideoAudio(4, 0, 1000, 600)),
			&media.VideoUsage{Duration: 4 * time.Second, Width: 1000, Height: 600, Audio: true}},
		{"unreadable", []byte("not an mp4"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := media.Job{
				ID: vertexOperation("proj"), Kind: media.KindVideo,
				Origin: p.origin("veo-3.1-fast-generate-001"),
			}
			poll := media.JobPoll{
				Job:   job,
				State: media.JobSucceeded,
				Content: []content.Block{content.VideoBlock(content.Source{
					Data: tc.data, MIMEType: "video/mp4",
				})},
			}
			result, err := p.FetchVideo(t.Context(), poll)
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Content[0].Video.Source.Data) != string(tc.data) {
				t.Fatal("video bytes changed")
			}
			if tc.want == nil {
				if len(result.VideoUsage) != 0 || result.Content[0].Video.Duration != 0 {
					t.Fatalf("unreadable file has usage %+v", result.VideoUsage)
				}
				return
			}
			if len(result.VideoUsage) != 1 || result.VideoUsage[0] != *tc.want ||
				result.Content[0].Video.Duration != tc.want.Duration {
				t.Fatalf("usage = %+v, want %+v", result.VideoUsage, *tc.want)
			}
		})
	}
	other := newVertexFixture(t, server)
	other.config.CredentialScope = "project:other"
	poll := media.JobPoll{
		Job: media.Job{
			ID: vertexOperation("proj"), Kind: media.KindVideo,
			Origin: other.origin("veo-3.1-fast-generate-001"),
		},
		State:   media.JobSucceeded,
		Content: []content.Block{content.VideoBlock(content.Source{Data: []byte("v"), MIMEType: "video/mp4"})},
	}
	if _, err := p.FetchVideo(t.Context(), poll); err == nil {
		t.Fatal("fetched a poll from another destination")
	}
	poll.Job.Origin = p.origin("veo-3.1-fast-generate-001")
	poll.State, poll.Content = media.JobPending, nil
	if _, err := p.FetchVideo(t.Context(), poll); err == nil {
		t.Fatal("fetched a pending poll")
	}
}

type fakeTokenProvider struct{ calls atomic.Int32 }

func (f *fakeTokenProvider) Token(context.Context) (*auth.Token, error) {
	f.calls.Add(1)
	return &auth.Token{Value: "fake-token", Type: "Bearer"}, nil
}

// rewriteTransport sends every request to target, so a client configured
// for a googleapis.com endpoint can reach a local server.
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestVertexVideoCredentials(t *testing.T) {
	clearVertexEnv(t)
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Clone())
		if r.Host != "us-central1-aiplatform.googleapis.com" {
			t.Errorf("host %s", r.Host)
		}
		fmt.Fprintf(w, `{"name":%q}`, vertexOperation("proj"))
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)

	p, err := NewVertexVideoJobs(VertexVideoConfig{Project: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	p.client = &http.Client{Transport: rewriteTransport{target}}
	var detections int
	p.detect = func() (*auth.Credentials, error) {
		detections++
		if detections == 1 {
			return nil, errors.New("no ADC")
		}
		return auth.NewCredentials(&auth.CredentialsOptions{
			TokenProvider: &fakeTokenProvider{},
			QuotaProjectIDProvider: auth.CredentialsPropertyFunc(
				func(context.Context) (string, error) { return "quota-proj", nil },
			),
		}), nil
	}
	request, err := p.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	// No ADC: rejected, with nothing sent.
	submission, err := p.SubmitVideo(t.Context(), request)
	providerErr, ok := errors.AsType[*media.Error](err)
	if submission.State != media.SubmissionRejected || !ok ||
		providerErr.Category != media.ErrorAuthentication || len(headers) != 0 {
		t.Fatalf("no ADC = %+v, %v, %d sent", submission, err, len(headers))
	}
	// The failed detection was not kept; the next send finds ADC and keeps it.
	for range 2 {
		if _, err := p.SubmitVideo(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	if detections != 2 || len(headers) != 2 {
		t.Fatalf("detections %d, sends %d", detections, len(headers))
	}
	for _, h := range headers {
		if h.Get("Authorization") != "Bearer fake-token" ||
			h.Get("X-Goog-User-Project") != "quota-proj" ||
			h.Get("X-Goog-Api-Key") != "" {
			t.Fatalf("headers = %v", h)
		}
	}

	// A supplied Client brings its own credentials; Dashi adds no token.
	headers = nil
	supplied, err := NewVertexVideoJobs(VertexVideoConfig{
		Project: "proj",
		Client:  &http.Client{Transport: rewriteTransport{target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	supplied.detect = func() (*auth.Credentials, error) {
		t.Error("detected ADC with a supplied Client")
		return nil, errors.New("unexpected")
	}
	request, _ = supplied.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
	if _, err := supplied.SubmitVideo(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || headers[0].Get("Authorization") != "" ||
		headers[0].Get("X-Goog-User-Project") != "" {
		t.Fatalf("supplied client headers = %v", headers)
	}
}

func TestVertexVideoRefusesRedirect(t *testing.T) {
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	p := newVertexFixture(t, server)
	request, _ := p.PrepareVideo(t.Context(), vertexInput(media.VideoSettings{}))
	if _, err := p.SubmitVideo(t.Context(), request); err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed: %v, %d hits", err, hits.Load())
	}
}
