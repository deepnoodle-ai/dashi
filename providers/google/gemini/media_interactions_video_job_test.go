package gemini

import (
	"context"
	"encoding/json"
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

func newInteractionsJobFixture(t *testing.T, server *httptest.Server) *InteractionsVideoJobClient {
	t.Helper()
	p, err := NewInteractionsVideoJobs(InteractionsMediaConfig{Provider: "google-test", APIKey: "fixture-key",
		Endpoint: server.URL + "/v1beta/interactions", UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInteractionsURIVideoLifecycle(t *testing.T) {
	var submitted, polled, fetched int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "fixture-key" {
			t.Error("missing API key")
		}
		switch r.URL.Path {
		case "/v1beta/interactions":
			submitted++
			var body struct {
				ResponseFormat struct {
					Delivery   string `json:"delivery"`
					Resolution string `json:"resolution"`
				} `json:"response_format"`
				Store bool `json:"store"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ResponseFormat.Delivery != "uri" || body.ResponseFormat.Resolution != "4k" || body.Store {
				t.Errorf("unexpected URI body: %+v", body)
			}
			fmt.Fprintf(w, `{"id":"interaction-1","model":"gemini-omni-1.1-flash","status":"completed","usage":{"total_tokens":99},"steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":%q}]}]}`,
				server.URL+"/v1beta/files/clip-1:download?alt=media")
		case "/v1beta/files/clip-1":
			polled++
			if polled == 1 {
				io.WriteString(w, `{"name":"files/clip-1","state":"PROCESSING"}`)
			} else {
				fmt.Fprintf(w, `{"name":"files/clip-1","state":"ACTIVE","mimeType":"video/mp4","downloadUri":%q,"videoMetadata":{"videoDuration":"3.5s"},"expirationTime":"2030-01-01T00:00:00Z"}`,
					server.URL+"/v1beta/files/clip-1:download?alt=media")
			}
		case "/v1beta/files/clip-1:download":
			if r.URL.Query().Get("alt") != "media" {
				t.Errorf("missing alt=media: %s", r.URL)
			}
			http.Redirect(w, r, "/v1beta/files/clip-1-final:download?alt=media", http.StatusFound)
		case "/v1beta/files/clip-1-final:download":
			fetched++
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "video-bytes")
		default:
			t.Errorf("unexpected path %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := newInteractionsJobFixture(t, server)
	request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "gemini-omni-1.1-flash",
		Prompt: []content.Block{content.TextBlock("a sunrise")}, Settings: media.VideoSettings{Resolution: "4k"}})
	if err != nil {
		t.Fatal(err)
	}
	if submitted != 0 {
		t.Fatal("preparation sent the request")
	}
	if _, err := p.client.SendVideo(context.Background(), request); err == nil {
		t.Fatal("inline call accepted URI request")
	}
	submission, err := p.SubmitVideo(context.Background(), request)
	if err != nil || submission.State != media.SubmissionAccepted || submission.Job == nil ||
		submission.ResponseID != "interaction-1" || submission.Served.Model != "gemini-omni-1.1-flash" ||
		!strings.Contains(string(submission.RawUsage), `"total_tokens":99`) || submission.Validate() != nil {
		t.Fatalf("submission: %+v, %v", submission, err)
	}
	first, err := p.PollVideo(context.Background(), *submission.Job)
	if err != nil || first.State != media.JobPending {
		t.Fatalf("pending poll: %+v, %v", first, err)
	}
	if _, err := p.FetchVideo(context.Background(), first); err == nil || fetched != 0 {
		t.Fatalf("fetch before ACTIVE: err=%v, fetched=%d", err, fetched)
	}
	second, err := p.PollVideo(context.Background(), *submission.Job)
	if err != nil || second.State != media.JobSucceeded || len(second.Content) != 1 ||
		second.Content[0].Video.Duration != 3500*time.Millisecond || second.Validate() != nil {
		t.Fatalf("ACTIVE poll: %+v, %v", second, err)
	}
	result, err := p.FetchVideo(context.Background(), second)
	if err != nil || len(result.Content) != 1 || string(result.Content[0].Video.Source.Data) != "video-bytes" ||
		result.Content[0].Video.Duration != 3500*time.Millisecond ||
		submitted != 1 || polled != 2 || fetched != 1 {
		t.Fatalf("fetch: %+v, %v; calls=%d/%d/%d", result, err, submitted, polled, fetched)
	}
}

func TestInteractionsURIVideoFailureAndForeignURI(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1beta/interactions":
				uri := server.URL + "/v1beta/files/clip-2:download?alt=media"
				if foreign {
					uri = "https://evil.example/v1beta/files/clip-2:download?alt=media"
				}
				fmt.Fprintf(w, `{"id":"interaction-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":%q}]}]}`, uri)
			case "/v1beta/files/clip-2":
				io.WriteString(w, `{"name":"files/clip-2","state":"FAILED","error":{"code":13,"message":"processing failed"}}`)
			default:
				t.Errorf("unexpected request %s", r.URL)
				http.NotFound(w, r)
			}
		}))
		p := newInteractionsJobFixture(t, server)
		request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "gemini-omni-1.1-flash",
			Prompt: []content.Block{content.TextBlock("a sunrise")}})
		if err != nil {
			t.Fatal(err)
		}
		submission, err := p.SubmitVideo(context.Background(), request)
		if foreign {
			if err == nil || submission.State != media.SubmissionUnknown || submission.Job != nil ||
				submission.ResponseID != "interaction-2" {
				t.Fatalf("foreign URI accepted: %+v, %v", submission, err)
			}
		} else {
			if err != nil || submission.Job == nil {
				t.Fatalf("submission: %+v, %v", submission, err)
			}
			poll, err := p.PollVideo(context.Background(), *submission.Job)
			if err != nil || poll.State != media.JobFailed || poll.Failure != "processing failed" {
				t.Fatalf("failed poll: %+v, %v", poll, err)
			}
		}
		server.Close()
	}
}

func TestInteractionsURICreationTerminalFailureIsNotRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"interaction-failed","model":"gemini-omni-1.1-flash","status":"failed","usage":{"total_tokens":2}}`)
	}))
	defer server.Close()
	p := newInteractionsJobFixture(t, server)
	request, err := p.PrepareVideo(context.Background(), media.VideoInput{Model: "gemini-omni-1.1-flash",
		Prompt: []content.Block{content.TextBlock("a sunrise")}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := p.SubmitVideo(context.Background(), request)
	if err == nil || submission.State != media.SubmissionUnknown || submission.Job != nil ||
		submission.ResponseID != "interaction-failed" || submission.Served.Model != "gemini-omni-1.1-flash" ||
		submission.Validate() != nil {
		t.Fatalf("terminal generation response lost or misclassified: %+v, %v", submission, err)
	}
}

func TestInteractionsURIJobDestinationBinding(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		io.WriteString(w, `{"name":"files/clip-3","state":"ACTIVE"}`)
	}))
	defer server.Close()
	first := newInteractionsJobFixture(t, server)
	second, err := NewInteractionsVideoJobs(InteractionsMediaConfig{Provider: "google-test", APIKey: "different-key",
		Endpoint: server.URL + "/v1beta/interactions", UnknownCapabilities: model.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	job := media.Job{ID: "files/clip-3", Kind: media.KindVideo, Origin: first.client.origin("gemini-omni-1.1-flash")}
	if _, err := second.PollVideo(context.Background(), job); err == nil || calls != 0 {
		t.Fatalf("foreign credential reached Files API: calls=%d, err=%v", calls, err)
	}
	job.ID = "files/../clip-3"
	if _, err := first.PollVideo(context.Background(), job); err == nil || calls != 0 {
		t.Fatalf("invalid File ID reached Files API: calls=%d, err=%v", calls, err)
	}
}
