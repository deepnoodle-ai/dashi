package gemini

import (
	"errors"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestMissingAPIKeyFailsAtSend(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	client, err := New(Config{})
	if err != nil {
		t.Fatalf("New without a key: %v", err)
	}
	_, err = client.Send(t.Context(), llm.Call{})
	e, ok := errors.AsType[*llm.Error](err)
	if !errors.Is(err, llm.ErrNoAPIKey) || !ok ||
		e.SendStatus != llm.SendStatusNotSent {
		t.Fatalf("Send error = %v, want a not-dispatched ErrNoAPIKey", err)
	}
	custom, err := New(Config{Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if custom.noKey != nil {
		t.Fatal("a custom HTTP client still requires a key")
	}
}

func TestMediaMissingAPIKeyFailsAtSend(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	mediaClient, err := NewMedia(MediaConfig{})
	if err != nil {
		t.Fatalf("NewMedia without a key: %v", err)
	}
	_, err = mediaClient.do(t.Context(), http.MethodPost, DefaultImageEndpoint, nil, "", 1, false)
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("media send error = %v, want ErrNoAPIKey", err)
	}
	interactions, err := NewInteractionsMedia(InteractionsMediaConfig{})
	if err != nil {
		t.Fatalf("NewInteractionsMedia without a key: %v", err)
	}
	if _, err := interactions.do(t.Context(), nil); !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("interactions send error = %v, want ErrNoAPIKey", err)
	}
	// Polling and fetching a video File also need the key, so they fail
	// before dispatch instead of sending an empty key header. Without a
	// configured client the request would use http.DefaultTransport, so
	// that transport fails the test if anything is sent.
	defaultTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("no-key video File call sent %s %s", r.Method, r.URL)
		return nil, errFixtureTransport
	})
	t.Cleanup(func() { http.DefaultTransport = defaultTransport })
	jobs, err := NewInteractionsVideoJobs(InteractionsMediaConfig{})
	if err != nil {
		t.Fatalf("NewInteractionsVideoJobs without a key: %v", err)
	}
	job := media.Job{
		ID:     "files/abc",
		Kind:   media.KindVideo,
		Origin: jobs.client.origin("veo"),
	}
	if _, err := jobs.PollVideo(t.Context(), job); !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("PollVideo error = %v, want ErrNoAPIKey", err)
	}
	poll := media.JobPoll{
		Job:   job,
		State: media.JobSucceeded,
		Content: []content.Block{content.VideoBlock(content.Source{
			URI:      jobs.fileURL(job.ID, true),
			MIMEType: "video/mp4",
			Origin:   job.Origin,
		})},
	}
	if _, err := jobs.FetchVideo(t.Context(), poll); !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("FetchVideo error = %v, want ErrNoAPIKey", err)
	}
	// A custom HTTP client may add credentials itself.
	custom, err := NewMedia(MediaConfig{
		Client: &http.Client{Transport: failingTransport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = custom.do(t.Context(), http.MethodPost, custom.imageURL("m"), nil, "", 1, false)
	if errors.Is(err, llm.ErrNoAPIKey) || !errors.Is(err, errFixtureTransport) {
		t.Fatalf("custom HTTP client error = %v, want the transport's", err)
	}
}

var errFixtureTransport = errors.New("fixture transport")

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errFixtureTransport
}

// Image and video endpoints are configured separately, so each operation
// checks the key for the endpoint it calls.
func TestMediaMissingKeyFollowsEachEndpoint(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	const (
		gatewayImage = "http://127.0.0.1:1/v1/models/{model}:generateContent"
		gatewayVideo = "http://127.0.0.1:1/v1beta/models/{model}:predictLongRunning"
	)
	cases := []struct {
		name                   string
		config                 MediaConfig
		imageNoKey, videoNoKey bool
	}{
		{
			name: "custom image, public video",
			config: MediaConfig{
				Provider:      "gateway",
				ImageEndpoint: gatewayImage,
			},
			videoNoKey: true,
		},
		{
			name: "public image, custom video",
			config: MediaConfig{
				Provider:      "gateway",
				VideoEndpoint: gatewayVideo,
			},
			imageNoKey: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewMedia(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.do(t.Context(), http.MethodPost, client.imageURL("m"), nil, "", 1, false)
			if got := errors.Is(err, llm.ErrNoAPIKey); got != tc.imageNoKey {
				t.Errorf("image send error = %v, want ErrNoAPIKey: %v", err, tc.imageNoKey)
			}
			_, err = client.do(t.Context(), http.MethodPost, client.videoURL("m"), nil, "", 1, true)
			if got := errors.Is(err, llm.ErrNoAPIKey); got != tc.videoNoKey {
				t.Errorf("video send error = %v, want ErrNoAPIKey: %v", err, tc.videoNoKey)
			}
		})
	}
}

func TestGeminiKeyStaysWithGoogle(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-secret")
	t.Setenv("GOOGLE_API_KEY", "")
	proxy, err := New(Config{
		Provider: "proxy",
		Endpoint: "https://proxy.example/v1beta/models/{model}:generateContent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.config.APIKey != "" {
		t.Fatal("a non-Google destination read GEMINI_API_KEY")
	}
	mediaProxy, err := NewMedia(MediaConfig{
		Provider:      "proxy",
		ImageEndpoint: "https://proxy.example/v1/models/{model}:generateContent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mediaProxy.config.APIKey != "" {
		t.Fatal("a non-Google media destination read GEMINI_API_KEY")
	}
	interactionsProxy, err := NewInteractionsMedia(InteractionsMediaConfig{
		Provider: "proxy",
		Endpoint: "https://proxy.example/v1beta/interactions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if interactionsProxy.config.APIKey != "" {
		t.Fatal("a non-Google Interactions destination read GEMINI_API_KEY")
	}
	google, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if google.config.APIKey != "gemini-secret" {
		t.Fatal("the Google destination did not read GEMINI_API_KEY")
	}
}
