package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/model"
	"github.com/deepnoodle-ai/dashi/speech"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

var fixtureMP3 = []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
var fixtureFrameMP3 = []byte{0xff, 0xfb, 0x90, 0x64, 0, 0, 0, 0}
var fixtureWAV = []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'A', 'V', 'E'}

type repeatingReader struct{}

func (repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func testClient(t *testing.T, f roundTripFunc) *Client {
	t.Helper()
	c, err := New(Config{APIKey: "test-key", Client: &http.Client{Transport: f}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func audioInput(data []byte, mimeType string) speech.TranscribeInput {
	return speech.TranscribeInput{
		Model: transcribeModel,
		Audio: content.AudioBlock(content.Source{
			Data: data, MIMEType: mimeType,
		}),
	}
}

func response(status int, ctype string, body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{ctype}},
		Body:       io.NopCloser(body),
	}
}

func TestTranscriptionPrepareSummaryAndWire(t *testing.T) {
	calls := 0
	original := slices.Clone(fixtureMP3)
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost ||
			r.URL.String() != endpoint+"/audio/transcriptions" ||
			r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" ||
			params["boundary"] == "" {
			t.Errorf("bad multipart content type: %q: %v", mediaType, err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if part.FormName() == "file" {
				if part.FileName() != "audio.mp3" ||
					part.Header.Get("Content-Type") != "audio/mpeg" ||
					!bytes.Equal(value, original) {
					t.Errorf("prepared upload changed: %q %q %x",
						part.FileName(), part.Header.Get("Content-Type"), value)
				}
			} else {
				fields[part.FormName()] = string(value)
			}
		}
		if fields["model"] != transcribeModel ||
			fields["response_format"] != "json" ||
			fields["language"] != "en" ||
			fields["prompt"] != "private prompt" {
			t.Errorf("wrong multipart fields: %v", fields)
		}
		return response(
			200, "application/json", strings.NewReader(`{"text":""}`),
		), nil
	})
	in := audioInput(slices.Clone(original), "audio/mpeg")
	in.Language, in.Prompt = "en", "private prompt"
	prepared, err := c.PrepareTranscription(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.Audio.Audio.Source.Data[0] = 0
	in.Prompt = "changed"
	summary := prepared.Summary()
	if summary.Origin.Provider != "openai" ||
		summary.Origin.API != TranscriptionsAPI ||
		summary.InputBytes != len(original) ||
		summary.InputMIMEType != "audio/mpeg" ||
		summary.ModelInfo.Evidence.Method != model.EvidenceDocumentation {
		t.Fatalf("wrong summary: %+v", summary)
	}
	summary.ModelInfo.Recognition.InputMIMETypes[0] = "mutated"
	if prepared.Summary().ModelInfo.Recognition.InputMIMETypes[0] != "audio/mpeg" {
		t.Fatal("summary shares capability slice")
	}
	serialized, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private prompt", "test-key", "ID3"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("summary exposed %q: %s", secret, serialized)
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", prepared, prepared), secret) {
			t.Fatalf("prepared request formatted with %q", secret)
		}
	}
	result, err := c.SendTranscription(t.Context(), prepared)
	if err != nil || result.Text != "" || calls != 1 ||
		result.Requested.Model != transcribeModel ||
		result.Served != (model.ModelRef{}) || result.ResponseID != "" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestSynthesisPrepareSummaryAndWire(t *testing.T) {
	calls := 0
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost ||
			r.URL.String() != endpoint+"/audio/speech" ||
			r.Header.Get("Authorization") != "Bearer test-key" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		var fields map[string]any
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Fatal(err)
		}
		if fields["model"] != synthesisModel || fields["input"] != "Private words" ||
			fields["voice"] != "alloy" || fields["instructions"] != "whisper" ||
			fields["response_format"] != "mp3" || fields["speed"] != float64(1) {
			t.Errorf("wrong speech body: %v", fields)
		}
		return response(200, "application/octet-stream",
			bytes.NewReader(fixtureFrameMP3)), nil
	})
	in := speech.SynthesizeInput{
		Model: synthesisModel, Text: "Private words", Instructions: "whisper",
	}
	prepared, err := c.PrepareSynthesis(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.Text = "changed"
	summary := prepared.Summary()
	if summary.Voice != "alloy" || summary.Speed != 1 ||
		summary.TextRunes != len("Private words") ||
		summary.OutputMIMEType != "audio/mpeg" ||
		summary.ModelInfo.Evidence.Method != model.EvidenceDocumentation {
		t.Fatalf("wrong summary: %+v", summary)
	}
	summary.ModelInfo.Synthesis.Voices[0] = "mutated"
	if prepared.Summary().ModelInfo.Synthesis.Voices[0] != "alloy" {
		t.Fatal("synthesis summary shares capability slice")
	}
	serialized, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Private words", "whisper", "test-key"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("summary exposed %q: %s", secret, serialized)
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", prepared, prepared), secret) {
			t.Fatalf("prepared request formatted with %q", secret)
		}
	}
	result, err := c.SendSynthesis(t.Context(), prepared)
	if err != nil || calls != 1 ||
		!bytes.Equal(result.Audio.Audio.Source.Data, fixtureFrameMP3) ||
		result.Audio.Audio.Source.MIMEType != "audio/mpeg" ||
		result.Served != (model.ModelRef{}) || result.ResponseID != "" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestTranscriptionPreflightRejectsUnsupportedInputs(t *testing.T) {
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("network request during Prepare")
		return nil, nil
	})
	base := audioInput(slices.Clone(fixtureMP3), "audio/mpeg")
	cases := []struct {
		name string
		edit func(*speech.TranscribeInput)
	}{
		{"model", func(in *speech.TranscribeInput) { in.Model = "whisper-1" }},
		{"timestamps", func(in *speech.TranscribeInput) {
			in.Granularity = speech.TimestampWord
		}},
		{"diarization", func(in *speech.TranscribeInput) { in.Diarization = true }},
		{"URL", func(in *speech.TranscribeInput) {
			in.Audio.Audio.Source = content.Source{URL: "https://example.com/a.mp3"}
		}},
		{"format", func(in *speech.TranscribeInput) {
			in.Audio.Audio.Format = &content.AudioFormat{
				Encoding: "pcm_s16le", SampleRateHz: 24000, Channels: 1,
			}
		}},
		{"declared mismatch", func(in *speech.TranscribeInput) {
			in.Audio.Audio.Source.MIMEType = "audio/wav"
		}},
		{"unknown bytes", func(in *speech.TranscribeInput) {
			in.Audio.Audio.Source.Data = []byte("unknown")
		}},
		{"invalid language", func(in *speech.TranscribeInput) {
			in.Language = "en-US"
		}},
		{"long prompt", func(in *speech.TranscribeInput) {
			in.Prompt = strings.Repeat("a", maxPromptBytes+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base.Clone()
			tc.edit(&in)
			if _, err := c.PrepareTranscription(t.Context(), in); err == nil {
				t.Fatal("unsupported input passed preparation")
			}
		})
	}
	tooLarge := audioInput(make([]byte, maxUploadBytes+1), "audio/mpeg")
	copy(tooLarge.Audio.Audio.Source.Data, fixtureMP3)
	if _, err := c.PrepareTranscription(t.Context(), tooLarge); err == nil {
		t.Fatal("oversized audio passed preparation")
	}
}

func TestTranscriptionFormatAndLimitBoundaries(t *testing.T) {
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("network request during Prepare")
		return nil, nil
	})
	for _, tc := range []struct {
		name, mimeType, filename string
		data                     []byte
	}{
		{"tagless MP3", "audio/mpeg", "audio.mp3", fixtureFrameMP3},
		{"WAV alias", "audio/x-wav", "audio.wav", fixtureWAV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := c.PrepareTranscription(t.Context(),
				audioInput(tc.data, tc.mimeType))
			if err != nil {
				t.Fatal(err)
			}
			r := request.(*preparedTranscription)
			if !bytes.Contains(r.body, []byte(`filename="`+tc.filename+`"`)) {
				t.Fatalf("wrong prepared filename: %s", r.body)
			}
		})
	}
	maxAudio := make([]byte, maxUploadBytes)
	copy(maxAudio, fixtureMP3)
	request, err := c.PrepareTranscription(t.Context(),
		audioInput(maxAudio, "audio/mpeg"))
	if err != nil || request.Summary().InputBytes != maxUploadBytes {
		t.Fatalf("25,000,000-byte boundary failed: %v", err)
	}
}

func TestSynthesisPreflightRejectsUnsupportedInputs(t *testing.T) {
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("network request during Prepare")
		return nil, nil
	})
	base := speech.SynthesizeInput{Model: synthesisModel, Text: "hello"}
	cases := []struct {
		name string
		edit func(*speech.SynthesizeInput)
	}{
		{"model", func(in *speech.SynthesizeInput) { in.Model = "tts-1" }},
		{"language", func(in *speech.SynthesizeInput) { in.Language = "en" }},
		{"format", func(in *speech.SynthesizeInput) {
			in.Format = &content.AudioFormat{
				Encoding: "pcm_s16le", SampleRateHz: 24000, Channels: 1,
			}
		}},
		{"WAV", func(in *speech.SynthesizeInput) { in.OutputMIMEType = "audio/wav" }},
		{"custom voice", func(in *speech.SynthesizeInput) { in.Voice = "voice_123" }},
		{"speed", func(in *speech.SynthesizeInput) {
			speed := 4.1
			in.Speed = &speed
		}},
		{"long text", func(in *speech.SynthesizeInput) {
			in.Text = strings.Repeat("é", 4097)
		}},
		{"long instructions", func(in *speech.SynthesizeInput) {
			in.Instructions = strings.Repeat("x", maxPromptBytes+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base.Clone()
			tc.edit(&in)
			if _, err := c.PrepareSynthesis(t.Context(), in); err == nil {
				t.Fatal("unsupported synthesis passed preparation")
			}
		})
	}
	base.Text = strings.Repeat("é", 4096)
	if _, err := c.PrepareSynthesis(t.Context(), base); err != nil {
		t.Fatalf("4096-rune boundary failed: %v", err)
	}
}

func TestSendRejectsWrongClientAndCanceledContext(t *testing.T) {
	c1 := testClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("network request on wrong client")
		return nil, nil
	})
	c2 := testClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("network request on canceled context")
		return nil, nil
	})
	request, err := c1.PrepareTranscription(t.Context(),
		audioInput(fixtureMP3, "audio/mpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.SendTranscription(t.Context(), request); err == nil {
		t.Fatal("another client accepted prepared request")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c1.SendTranscription(ctx, request); err == nil {
		t.Fatal("canceled send dispatched")
	}
}

func TestSendTranscriptionFailureClassesAndNoRetry(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		ctype          string
		body           string
		transportError error
		category       speech.ErrorCategory
		outcomeUnknown bool
	}{
		{"missing text", 200, "application/json", `{}`, nil,
			speech.ErrorProtocol, true},
		{"null text", 200, "application/json", `{"text":null}`, nil,
			speech.ErrorProtocol, true},
		{"wrong text", 200, "application/json", `{"text":9}`, nil,
			speech.ErrorProtocol, true},
		{"wrong media", 200, "text/plain", `{"text":"ok"}`, nil,
			speech.ErrorProtocol, true},
		{"oversized JSON", 200, "application/json",
			strings.Repeat("x", maxJSONBytes+1), nil,
			speech.ErrorProtocol, true},
		{"auth", 401, "application/json", `{}`, nil,
			speech.ErrorAuthentication, false},
		{"rate limit", 429, "application/json", `{}`, nil,
			speech.ErrorRateLimited, false},
		{"server error", 503, "application/json", `{}`, nil,
			speech.ErrorProvider, true},
		{"lost response", 0, "", "", errors.New("lost"),
			speech.ErrorTransport, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := testClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				if tc.transportError != nil {
					return nil, tc.transportError
				}
				return response(tc.status, tc.ctype, strings.NewReader(tc.body)), nil
			})
			request, err := c.PrepareTranscription(t.Context(),
				audioInput(fixtureMP3, "audio/mpeg"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.SendTranscription(t.Context(), request)
			var speechErr *speech.Error
			if !errors.As(err, &speechErr) || calls != 1 ||
				speechErr.Category != tc.category ||
				speechErr.RemoteOutcomeUnknown != tc.outcomeUnknown {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestDocumentationEvidenceAndMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	c, err := New(Config{Client: &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			t.Fatal("missing key dispatched a request")
			return nil, nil
		})}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{transcribeModel, synthesisModel} {
		info, found := c.ModelInfo(name)
		if !found || info.Evidence.Method != model.EvidenceDocumentation ||
			info.Evidence.Endpoint != endpoint || info.Validate() != nil {
			t.Fatalf("invalid documentation evidence for %q: %+v", name, info)
		}
	}
	if _, found := c.ModelInfo("other"); found {
		t.Fatal("unknown model inherited documentation evidence")
	}
	request, err := c.PrepareSynthesis(t.Context(),
		speech.SynthesizeInput{Model: synthesisModel, Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SendSynthesis(t.Context(), request)
	var speechErr *speech.Error
	if !errors.As(err, &speechErr) ||
		speechErr.Category != speech.ErrorAuthentication ||
		speechErr.RemoteOutcomeUnknown {
		t.Fatalf("missing key error: %v", err)
	}
}

func TestSendSynthesisRejectsWrongBodyAndBoundsResponse(t *testing.T) {
	for _, tc := range []struct {
		name, ctype string
		body        io.Reader
	}{
		{"wrong media", "audio/wav", bytes.NewReader(fixtureMP3)},
		{"wrong bytes", "application/octet-stream", strings.NewReader("not audio")},
		{"empty", "audio/mpeg", strings.NewReader("")},
		{"too large", "audio/mpeg", io.LimitReader(
			repeatingReader{}, maxResponseBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(*http.Request) (*http.Response, error) {
				return response(200, tc.ctype, tc.body), nil
			})
			request, err := c.PrepareSynthesis(t.Context(),
				speech.SynthesizeInput{Model: synthesisModel, Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.SendSynthesis(t.Context(), request)
			var speechErr *speech.Error
			if !errors.As(err, &speechErr) ||
				speechErr.Category != speech.ErrorProtocol ||
				!speechErr.RemoteOutcomeUnknown {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
