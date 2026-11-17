package gemini

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestOmniDurationIsSentAsWholeSeconds(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	input := media.VideoInput{
		Model:    "gemini-omni-1.2-flash",
		Prompt:   []content.Block{content.TextBlock("a scene")},
		Settings: media.VideoSettings{Duration: 15 * time.Second},
	}
	request, err := p.PrepareVideo(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ResponseFormat map[string]any `json:"response_format"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ResponseFormat["duration"] != "15s" {
		t.Fatalf("response_format = %v", body.ResponseFormat)
	}
	for _, d := range []time.Duration{
		2 * time.Second,
		16 * time.Second,
		3500 * time.Millisecond,
	} {
		input.Settings.Duration = d
		if _, err := p.PrepareVideo(t.Context(), input); err == nil {
			t.Errorf("duration %s was accepted", d)
		}
	}
}

func TestOmniFlashPreviewIsShutDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	if _, err := p.PrepareVideo(t.Context(), media.VideoInput{
		Model:  "gemini-omni-flash-preview",
		Prompt: []content.Block{content.TextBlock("a scene")},
	}); err == nil {
		t.Fatal("gemini-omni-flash-preview was accepted")
	}
}

// omniBody is the part of a prepared Omni body that the mapping tests read.
type omniBody struct {
	Input []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MIMEType string `json:"mime_type"`
		Data     string `json:"data"`
		URI      string `json:"uri"`
	} `json:"input"`
	GenerationConfig struct {
		VideoConfig struct {
			Task string `json:"task"`
		} `json:"video_config"`
	} `json:"generation_config"`
	ResponseFormat map[string]any `json:"response_format"`
	Store          bool           `json:"store"`
}

func decodeOmniBody(t *testing.T, request media.VideoRequest) omniBody {
	t.Helper()
	var body omniBody
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func prepareOmni(
	t *testing.T,
	p *InteractionsMediaClient,
	in media.VideoInput,
) (omniBody, error) {
	t.Helper()
	request, err := p.PrepareVideo(t.Context(), in)
	if err != nil {
		return omniBody{}, err
	}
	return decodeOmniBody(t, request), nil
}

func omniImage(data string) content.Block {
	return content.ImageBlock(content.Source{Data: []byte(data), MIMEType: "image/png"})
}

func omniVideo(data string, duration time.Duration) content.Block {
	block := content.VideoBlock(content.Source{Data: []byte(data), MIMEType: "video/mp4"})
	block.Video.Duration = duration
	return block
}

func omniPartTypes(body omniBody) string {
	var types []string
	for _, part := range body.Input {
		types = append(types, part.Type)
	}
	return strings.Join(types, ",")
}

func TestOmniFirstAndLastFrames(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	in := media.VideoInput{
		Model:      "gemini-omni-1.2-flash",
		Prompt:     []content.Block{content.TextBlock("the cup turns into a bird")},
		FirstFrame: new(omniImage("first")),
		LastFrame:  new(omniImage("last")),
		Settings:   media.VideoSettings{Duration: 10 * time.Second},
	}
	body, err := prepareOmni(t, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if omniPartTypes(body) != "image,image,text" ||
		body.Input[0].Data != base64.StdEncoding.EncodeToString([]byte("first")) ||
		body.Input[1].Data != base64.StdEncoding.EncodeToString([]byte("last")) ||
		body.GenerationConfig.VideoConfig.Task != "image_to_video" {
		t.Fatalf("first and last frames encoded as %+v", body)
	}

	in.Settings.Duration = 11 * time.Second
	if _, err := prepareOmni(t, p, in); err == nil ||
		!strings.Contains(err.Error(), "3 to 10 s") {
		t.Fatalf("11 s with a last frame: %v", err)
	}

	// A first frame alone needs no prompt.
	body, err = prepareOmni(t, p, media.VideoInput{
		Model:      "gemini-omni-1.2-flash",
		FirstFrame: new(omniImage("first")),
	})
	if err != nil || omniPartTypes(body) != "image" ||
		body.GenerationConfig.VideoConfig.Task != "image_to_video" {
		t.Fatalf("first frame only: %+v %v", body, err)
	}

	// Text-to-video sends no task.
	body, err = prepareOmni(t, p, media.VideoInput{
		Model:  "gemini-omni-1.2-flash",
		Prompt: []content.Block{content.TextBlock("a scene")},
	})
	if err != nil || omniPartTypes(body) != "text" ||
		body.GenerationConfig.VideoConfig.Task != "" {
		t.Fatalf("text only: %+v %v", body, err)
	}
}

func TestOmniReferenceImagesAndVideos(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	prompt := []content.Block{content.TextBlock("the woman from the photos walks a dog")}
	body, err := prepareOmni(t, p, media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		Prompt:          prompt,
		ReferenceImages: []content.Block{omniImage("a"), omniImage("b")},
		ReferenceVideos: []content.Block{omniVideo("clip", 3*time.Second)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if omniPartTypes(body) != "image,image,video,text" ||
		body.Input[2].MIMEType != "video/mp4" ||
		body.GenerationConfig.VideoConfig.Task != "reference_to_video" {
		t.Fatalf("references encoded as %+v", body)
	}

	sixImages := make([]content.Block, 6)
	for i := range sixImages {
		sixImages[i] = omniImage("ref")
	}
	if _, err := prepareOmni(t, p, media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		Prompt:          prompt,
		ReferenceImages: sixImages,
	}); err != nil {
		t.Fatalf("six reference images: %v", err)
	}
	threeVideos := []content.Block{
		omniVideo("a", 0), omniVideo("b", 0), omniVideo("c", 0),
	}
	if _, err := prepareOmni(t, p, media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		Prompt:          prompt,
		ReferenceVideos: threeVideos,
	}); err != nil {
		t.Fatalf("three reference videos at the default duration: %v", err)
	}
	if _, err := prepareOmni(t, p, media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		Prompt:          prompt,
		ReferenceVideos: threeVideos[:1],
		Settings:        media.VideoSettings{Duration: 15 * time.Second},
	}); err != nil {
		t.Fatalf("one reference video at 15 s: %v", err)
	}

	for _, tc := range []struct {
		name string
		in   media.VideoInput
		want string
	}{
		{"seven reference images", media.VideoInput{
			ReferenceImages: append(slices.Clone(sixImages), omniImage("seventh")),
		}, "at most 6"},
		{"two reference videos at 11 s", media.VideoInput{
			ReferenceVideos: threeVideos[:2],
			Settings:        media.VideoSettings{Duration: 11 * time.Second},
		}, "at most 1"},
		{"four reference videos", media.VideoInput{
			ReferenceVideos: append(slices.Clone(threeVideos), omniVideo("d", 0)),
		}, "at most 3"},
		{"long reference video", media.VideoInput{
			ReferenceVideos: []content.Block{omniVideo("long", 4*time.Second)},
		}, "up to 3s"},
		{"first frame with reference image", media.VideoInput{
			FirstFrame:      new(omniImage("first")),
			ReferenceImages: []content.Block{omniImage("ref")},
		}, "cannot combine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Model = "gemini-omni-1.2-flash"
			tc.in.Prompt = prompt
			if _, err := prepareOmni(t, p, tc.in); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	if _, err := prepareOmni(t, p, media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		ReferenceImages: []content.Block{omniImage("ref")},
	}); err == nil || !strings.Contains(err.Error(), "text prompt") {
		t.Fatalf("references without a prompt: %v", err)
	}
}

func TestOmniEdit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsFixture(t, server)
	source := omniVideo("source", 8*time.Second)
	in := media.VideoInput{
		Model:           "gemini-omni-1.2-flash",
		Operation:       media.VideoEdit,
		Prompt:          []content.Block{content.TextBlock("make the cup red")},
		SourceVideo:     &source,
		ReferenceImages: []content.Block{omniImage("red cup")},
		Settings:        media.VideoSettings{Resolution: "720p"},
	}
	body, err := prepareOmni(t, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if omniPartTypes(body) != "video,image,text" ||
		body.Input[0].Data != base64.StdEncoding.EncodeToString([]byte("source")) ||
		body.GenerationConfig.VideoConfig.Task != "edit" ||
		body.ResponseFormat["resolution"] != "720p" || body.Store {
		t.Fatalf("edit encoded as %+v", body)
	}

	long := omniVideo("long", 16*time.Second)
	for _, tc := range []struct {
		name   string
		change func(*media.VideoInput)
		want   string
	}{
		{"duration", func(in *media.VideoInput) {
			in.Settings.Duration = 5 * time.Second
		}, "keeps the source duration"},
		{"aspect ratio", func(in *media.VideoInput) {
			in.Settings.AspectRatio = "9:16"
		}, "aspect ratio"},
		{"long source", func(in *media.VideoInput) {
			in.SourceVideo = &long
		}, "up to 15s"},
		{"no source", func(in *media.VideoInput) {
			in.SourceVideo = nil
		}, "requires SourceVideo"},
		{"first frame", func(in *media.VideoInput) {
			in.FirstFrame = new(omniImage("first"))
		}, "not frames"},
		{"reference video", func(in *media.VideoInput) {
			in.ReferenceVideos = []content.Block{omniVideo("clip", 0)}
		}, "not frames or reference videos"},
		{"video in prompt", func(in *media.VideoInput) {
			in.Prompt = append(in.Prompt, omniVideo("clip", 0))
		}, "prompt block"},
		{"source without MIME type", func(in *media.VideoInput) {
			in.SourceVideo = new(content.VideoBlock(content.Source{Data: []byte("??")}))
		}, "video MIME type"},
		{"source over plain HTTP", func(in *media.VideoInput) {
			in.SourceVideo = new(content.VideoBlock(content.Source{
				URL: "http://example.test/v.mp4",
			}))
		}, "HTTPS Files API URI"},
		{"another provider's file", func(in *media.VideoInput) {
			in.SourceVideo = new(content.VideoBlock(content.Source{
				URI:      "https://example.test/files/abc",
				MIMEType: "video/mp4",
				Origin: content.Origin{
					Provider: "other",
					API:      InteractionsAPI,
					Scope:    "scope",
				},
			}))
		}, `provider "other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := in.Clone()
			tc.change(&changed)
			if _, err := prepareOmni(t, p, changed); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestOmniExtend(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	p := newInteractionsJobFixture(t, server)
	const model = "gemini-omni-1.2-flash"
	// The shape of a URI-delivered Omni output from FetchVideo's poll.
	const uri = "https://generativelanguage.googleapis.com/v1beta/files/abc:download?alt=media"
	source := content.VideoBlock(content.Source{
		URI:      uri,
		MIMEType: "video/mp4",
		Origin:   p.client.origin(model),
	})
	source.Video.Duration = 20 * time.Second
	in := media.VideoInput{
		Model:       model,
		Operation:   media.VideoExtend,
		Prompt:      []content.Block{content.TextBlock("continue the scene")},
		SourceVideo: &source,
		Settings:    media.VideoSettings{Duration: 5 * time.Second, Resolution: "1080p"},
	}
	request, err := p.PrepareVideo(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeOmniBody(t, request)
	if omniPartTypes(body) != "video,text" ||
		body.Input[0].URI != uri || body.Input[0].Data != "" ||
		body.GenerationConfig.VideoConfig.Task != "extend" ||
		body.ResponseFormat["duration"] != "5s" ||
		body.ResponseFormat["delivery"] != "uri" || !body.Store {
		t.Fatalf("extension encoded as %+v", body)
	}

	for _, tc := range []struct {
		name     string
		source   time.Duration
		duration time.Duration
		want     string
	}{
		// Unset duration adds Omni's default 10 s: 20 + 10 = 30 s fits.
		{"default added length", 20 * time.Second, 0, ""},
		{"unknown source length", 0, 10 * time.Second, ""},
		{"too much added", 20 * time.Second, 11 * time.Second, "adds 3 to 10 s"},
		{"over 30 s total", 25 * time.Second, 0, "exceeds 30s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := in.Clone()
			changed.Settings.Duration = tc.duration
			changed.SourceVideo.Video.Duration = tc.source
			_, err := p.PrepareVideo(t.Context(), changed)
			if tc.want == "" && err != nil ||
				tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
