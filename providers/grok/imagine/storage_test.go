package imagine

import (
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

func TestImageStoragePreservesEachOutputAndPublicURLFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Model          string `json:"model"`
			StorageOptions struct {
				Filename     string `json:"filename"`
				ExpiresAfter int    `json:"expires_after"`
				PublicURL    struct {
					ExpiresAfter int `json:"expires_after"`
				} `json:"public_url"`
			} `json:"storage_options"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Error(err)
		}
		if wire.Model != "grok-imagine-image-2.0" ||
			wire.StorageOptions.Filename != "scene.jpg" ||
			wire.StorageOptions.ExpiresAfter != 7200 ||
			wire.StorageOptions.PublicURL.ExpiresAfter != 3600 {
			t.Errorf("unexpected storage request: %s", body)
		}
		_, _ = io.WriteString(
			w,
			`{"data":[`+
				`{"url":"https://imgen.x.ai/first.jpg","mime_type":"image/jpeg",`+
				`"file_output":{"file_id":"file_first","filename":"scene.jpg","expires_at":1800000000,`+
				`"public_url":"https://files-cdn.x.ai/first.jpg","public_url_expires_at":1799990000}},`+
				`{"url":"https://imgen.x.ai/second.jpg","mime_type":"image/jpeg",`+
				`"file_output":{"file_id":"file_second","filename":"scene.jpg",`+
				`"public_url_error":"Public URL quota reached; file stored."}}]}`,
		)
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	request, err := client.PrepareImageWithOptions(
		t.Context(),
		media.ImageInput{
			Model:     "grok-imagine-image-2.0",
			Operation: media.ImageGenerate,
			Prompt:    []content.Block{content.TextBlock("Two quiet scenes")},
			Settings:  media.ImageSettings{Count: 2},
		},
		ImageOptions{
			Storage: &StorageOptions{
				Filename:              "scene.jpg",
				ExpiresAfter:          2 * time.Hour,
				PublicURL:             true,
				PublicURLExpiresAfter: time.Hour,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Snapshot().ProviderOptions) == 0 {
		t.Fatal("storage options were not frozen in request")
	}
	result, err := client.SendImage(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || len(result.StoredOutputs) != 2 {
		t.Fatalf("result outputs = %+v", result)
	}
	first, second := result.StoredOutputs[0], result.StoredOutputs[1]
	if first.ContentIndex != 0 ||
		first.File.ID != "file_first" ||
		first.File.Origin != client.FileOrigin() ||
		first.PublicURL == nil ||
		first.PublicURL.URL != "https://files-cdn.x.ai/first.jpg" ||
		first.File.ExpiresAt == nil || first.PublicURL.ExpiresAt == nil {
		t.Fatalf("first stored output = %+v", first)
	}
	if second.ContentIndex != 1 || second.File.ID != "file_second" ||
		second.PublicURL != nil ||
		second.PublicURLError != "Public URL quota reached; file stored." ||
		result.Content[1].Image.Source.URL != "https://imgen.x.ai/second.jpg" {
		t.Fatalf("second stored output = %+v", second)
	}
}

func TestVideoStorageSurvivesPollAndFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos/generations":
			body, _ := io.ReadAll(r.Body)
			var wire struct {
				StorageOptions map[string]any `json:"storage_options"`
			}
			if err := json.Unmarshal(
				body,
				&wire,
			); err != nil ||
				wire.StorageOptions["filename"] != "clip.mp4" ||
				wire.StorageOptions["public_url"] != nil {
				t.Errorf("unexpected private storage request: %s, %v", body, err)
			}
			_, _ = io.WriteString(w, `{"request_id":"video-stored"}`)
		case "/v1/videos/video-stored":
			_, _ = io.WriteString(
				w,
				`{"status":"done","model":"grok-imagine-video-1.5",`+
					`"video":{"url":"https://vidgen.x.ai/clip.mp4","duration":5,`+
					`"file_output":{"file_id":"file_clip","filename":"clip.mp4",`+
					`"public_url_error":"Public URL creation timed out."}}}`,
			)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := testClient(
		t,
		server.URL+"/v1",
		server.Client(),
		model.CapabilityStrict,
	)
	request, err := client.PrepareVideoWithOptions(
		t.Context(),
		media.VideoInput{
			Model:  "grok-imagine-video-1.5",
			Prompt: []content.Block{content.TextBlock("A ball bouncing")},
		},
		VideoOptions{Storage: &StorageOptions{Filename: "clip.mp4"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := client.SubmitVideo(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := client.PollVideo(t.Context(), *submitted.Job)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.FetchVideo(t.Context(), poll)
	if err != nil {
		t.Fatal(err)
	}
	if poll.State != media.JobSucceeded ||
		len(poll.StoredOutputs) != 1 ||
		len(result.StoredOutputs) != 1 ||
		poll.StoredOutputs[0].File.ID != "file_clip" ||
		result.StoredOutputs[0].File.Origin != client.FileOrigin() ||
		result.StoredOutputs[0].PublicURLError != "Public URL creation timed out." ||
		result.Content[0].Video.Source.URL != "https://vidgen.x.ai/clip.mp4" {
		t.Fatalf("stored video was lost: poll=%+v result=%+v", poll, result)
	}
}

func TestStorageOptionsRejectInvalidConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options StorageOptions
	}{
		{"missing filename", StorageOptions{}},
		{"path filename", StorageOptions{Filename: "../image.jpg"}},
		{
			"fractional expiry",
			StorageOptions{
				Filename:     "image.jpg",
				ExpiresAfter: time.Hour + time.Millisecond,
			},
		},
		{
			"short expiry",
			StorageOptions{Filename: "image.jpg", ExpiresAfter: time.Minute},
		},
		{
			"long expiry",
			StorageOptions{Filename: "image.jpg", ExpiresAfter: 31 * 24 * time.Hour},
		},
		{
			"public expiry without URL",
			StorageOptions{Filename: "image.jpg", PublicURLExpiresAfter: time.Hour},
		},
		{
			"public URL outlives file",
			StorageOptions{
				Filename:              "image.jpg",
				ExpiresAfter:          time.Hour,
				PublicURL:             true,
				PublicURLExpiresAfter: 2 * time.Hour,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.options.wire(); err == nil {
				t.Fatal("invalid storage options accepted")
			}
		})
	}
}

func TestOutputExpiryAcceptsUnixSecondsAndRFC3339(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`1800000000`),
		json.RawMessage(`"2027-01-15T08:00:00Z"`),
	} {
		if parseOutputExpiry(raw) == nil {
			t.Fatalf("expiry was not parsed: %s", raw)
		}
	}
}
