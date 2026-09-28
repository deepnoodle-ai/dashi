// Package speech implements OpenAI's dedicated batch audio transcription and
// speech endpoints. Each prepared request owns its wire bytes and Send makes
// one attempt; streaming and realtime sessions are separate operations.
package speech

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/model"
	"github.com/deepnoodle-ai/dashi/speech"
)

const (
	TranscriptionsAPI = "openai.audio.transcriptions"
	SpeechAPI         = "openai.audio.speech"
	endpoint          = "https://api.openai.com/v1"
	transcribeModel   = "gpt-4o-mini-transcribe"
	synthesisModel    = "gpt-4o-mini-tts"
	maxUploadBytes    = 25_000_000
	maxFormOverhead   = 16 << 10
	maxResponseBytes  = 64 << 20
	maxJSONBytes      = 1 << 20
	maxPromptBytes    = 8 << 10
	maxErrorBytes     = 64 << 10
)

var documentationDate = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

var builtInVoices = []string{
	"alloy", "ash", "ballad", "coral", "echo", "fable", "onyx",
	"nova", "sage", "shimmer", "verse", "marin", "cedar",
}

// Config chooses credentials and an optional HTTP transport for the public
// OpenAI endpoint. A custom endpoint needs separately scoped model evidence.
type Config struct {
	APIKey string
	Client *http.Client
}

// Client is safe for concurrent batch calls. It has no mutable per-call state.
type Client struct {
	apiKey string
	http   http.Client
	scope  string
}

func (*Client) String() string {
	return "openai.speech.Client{credentials redacted}"
}

func (c *Client) GoString() string { return c.String() }

var _ speech.Recognizer = (*Client)(nil)
var _ speech.Synthesizer = (*Client)(nil)

// New constructs a public-endpoint client. Missing credentials are permitted
// for offline preparation; Send then fails before dispatch.
func New(config Config) (*Client, error) {
	key := config.APIKey
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if strings.TrimSpace(key) != key {
		return nil, errors.New("speech: API key has surrounding whitespace")
	}
	httpClient := http.Client{Timeout: 5 * time.Minute}
	if config.Client != nil {
		httpClient = *config.Client
		if httpClient.Timeout <= 0 {
			httpClient.Timeout = 5 * time.Minute
		}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	scopeBytes, _ := json.Marshal([]string{endpoint, key})
	scopeHash := sha256.Sum256(scopeBytes)
	return &Client{apiKey: key, http: httpClient,
		scope: hex.EncodeToString(scopeHash[:])}, nil
}

// ModelInfo returns documentation evidence only for the two qualified wire
// shapes at the public /v1 base endpoint. Evidence.API distinguishes the
// operation and Send appends its path. It does not claim a live probe.
func (*Client) ModelInfo(name string) (speech.ModelInfo, bool) {
	switch name {
	case transcribeModel:
		return speech.ModelInfo{
			Recognition: &speech.RecognitionCapabilities{
				Batch:          model.SupportSupported,
				InputMIMETypes: []string{"audio/mpeg", "audio/wav"},
			},
			Evidence: model.ModelEvidence{
				Model: name, API: TranscriptionsAPI, Endpoint: endpoint,
				Owner: "Dashi", Method: model.EvidenceDocumentation,
				Source:      "https://developers.openai.com/api/docs/guides/speech-to-text",
				QualifiedAt: documentationDate,
			},
		}, true
	case synthesisModel:
		return speech.ModelInfo{
			Synthesis: &speech.SynthesisCapabilities{
				Batch:           model.SupportSupported,
				Voices:          slices.Clone(builtInVoices),
				OutputMIMETypes: []string{"audio/mpeg"},
			},
			Evidence: model.ModelEvidence{
				Model: name, API: SpeechAPI, Endpoint: endpoint,
				Owner: "Dashi", Method: model.EvidenceDocumentation,
				Source: "https://developers.openai.com/api/reference/cli/resources/" +
					"audio/subresources/speech/methods/create",
				QualifiedAt: documentationDate,
			},
		}, true
	}
	return speech.ModelInfo{}, false
}

type preparedTranscription struct {
	client  *Client
	summary speech.TranscriptionSummary
	body    []byte
	ctype   string
}

func (*preparedTranscription) String() string {
	return "speech.TranscriptionRequest{body redacted}"
}

func (r *preparedTranscription) GoString() string { return r.String() }

func (r *preparedTranscription) Summary() speech.TranscriptionSummary {
	s := r.summary
	s.ModelInfo = s.ModelInfo.Clone()
	return s
}

type preparedSynthesis struct {
	client  *Client
	summary speech.SynthesisSummary
	body    []byte
}

func (*preparedSynthesis) String() string {
	return "speech.SynthesisRequest{body redacted}"
}

func (r *preparedSynthesis) GoString() string { return r.String() }

func (r *preparedSynthesis) Summary() speech.SynthesisSummary {
	s := r.summary
	s.ModelInfo = s.ModelInfo.Clone()
	return s
}

// PrepareTranscription validates one complete inline recording and freezes a
// multipart request without remote work.
func (c *Client) PrepareTranscription(ctx context.Context,
	in speech.TranscribeInput) (speech.TranscriptionRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, fail(speech.ErrorCanceled, err)
	}
	if err := in.Validate(); err != nil {
		return nil, fail(speech.ErrorInvalidRequest, err)
	}
	if in.Model != transcribeModel || in.Granularity != "" || in.Diarization {
		return nil, fail(speech.ErrorUnsupported,
			errors.New("model, timestamps, or diarization unsupported by this route"))
	}
	source := in.Audio.Audio.Source
	if source.Data == nil || in.Audio.Audio.Format != nil {
		return nil, fail(speech.ErrorUnsupported,
			errors.New("transcription requires inline containerized audio"))
	}
	if len(source.Data) > maxUploadBytes {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("audio exceeds 25,000,000 bytes"))
	}
	format, err := identifyAudio(source.Data)
	if err != nil {
		return nil, fail(speech.ErrorUnsupported, err)
	}
	if source.MIMEType != "" && normalizeMIME(source.MIMEType) != format.mimeType {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("declared audio MIME disagrees with bytes"))
	}
	if in.Language != "" && !validLanguage(in.Language) {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("language must be a lowercase two-letter code"))
	}
	if !utf8.ValidString(in.Prompt) || len(in.Prompt) > maxPromptBytes {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("transcription prompt must be UTF-8 and at most 8192 bytes"))
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(
		`form-data; name="file"; filename="audio.%s"`, format.extension))
	header.Set("Content-Type", format.mimeType)
	part, err := writer.CreatePart(header)
	if err == nil {
		_, err = part.Write(source.Data)
	}
	if err == nil {
		err = writer.WriteField("model", in.Model)
	}
	if err == nil {
		err = writer.WriteField("response_format", "json")
	}
	if err == nil && in.Language != "" {
		err = writer.WriteField("language", in.Language)
	}
	if err == nil && in.Prompt != "" {
		err = writer.WriteField("prompt", in.Prompt)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return nil, fail(speech.ErrorInvalidRequest, err)
	}
	if body.Len() > maxUploadBytes+maxFormOverhead {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("prepared multipart body exceeds bound"))
	}
	info, _ := c.ModelInfo(in.Model)
	return &preparedTranscription{
		client: c, body: body.Bytes(), ctype: writer.FormDataContentType(),
		summary: speech.TranscriptionSummary{
			Origin:        c.origin(TranscriptionsAPI, in.Model),
			InputMIMEType: format.mimeType, InputBytes: len(source.Data),
			Language: in.Language, ModelInfo: info,
		},
	}, nil
}

// PrepareSynthesis validates text and voice controls and freezes one MP3 call.
func (c *Client) PrepareSynthesis(ctx context.Context,
	in speech.SynthesizeInput) (speech.SynthesisRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, fail(speech.ErrorCanceled, err)
	}
	if err := in.Validate(); err != nil {
		return nil, fail(speech.ErrorInvalidRequest, err)
	}
	if in.Model != synthesisModel || in.Language != "" || in.Format != nil ||
		(in.OutputMIMEType != "" && in.OutputMIMEType != "audio/mpeg") {
		return nil, fail(speech.ErrorUnsupported,
			errors.New("model, language, or output format unsupported by this route"))
	}
	if !utf8.ValidString(in.Text) || utf8.RuneCountInString(in.Text) > 4096 ||
		!utf8.ValidString(in.Instructions) || len(in.Instructions) > maxPromptBytes {
		return nil, fail(speech.ErrorInvalidRequest,
			errors.New("speech text/instructions exceed UTF-8 limits"))
	}
	voice := in.Voice
	if voice == "" {
		voice = "alloy"
	}
	if !slices.Contains(builtInVoices, voice) {
		return nil, fail(speech.ErrorUnsupported,
			errors.New("voice is not a supported built-in voice"))
	}
	speed := 1.0
	if in.Speed != nil {
		speed = *in.Speed
	}
	if speed < 0.25 || speed > 4 {
		return nil, fail(speech.ErrorUnsupported,
			errors.New("speed must be between 0.25 and 4.0"))
	}
	requestBody := struct {
		Model          string  `json:"model"`
		Input          string  `json:"input"`
		Voice          string  `json:"voice"`
		Instructions   string  `json:"instructions,omitempty"`
		Speed          float64 `json:"speed"`
		ResponseFormat string  `json:"response_format"`
	}{in.Model, in.Text, voice, in.Instructions, speed, "mp3"}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fail(speech.ErrorInvalidRequest, err)
	}
	info, _ := c.ModelInfo(in.Model)
	return &preparedSynthesis{
		client: c, body: body,
		summary: speech.SynthesisSummary{
			Origin:         c.origin(SpeechAPI, in.Model),
			OutputMIMEType: "audio/mpeg", TextRunes: utf8.RuneCountInString(in.Text),
			Voice: voice, Speed: speed, ModelInfo: info,
		},
	}, nil
}

func (c *Client) origin(api, name string) content.Origin {
	return content.Origin{
		Provider: "openai",
		API:      api,
		Model:    name,
		Scope:    c.scope,
	}
}

func fail(category speech.ErrorCategory, err error) error {
	return &speech.Error{Category: category, Cause: err}
}

func validLanguage(language string) bool {
	if len(language) != 2 {
		return false
	}
	for _, c := range language {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func normalizeMIME(value string) string {
	typeName, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	switch typeName {
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	}
	return typeName
}

func (c *Client) do(ctx context.Context, path, ctype string,
	body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, fail(speech.ErrorInvalidRequest, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", ctype)
	response, err := c.http.Do(request)
	if err != nil {
		category := speech.ErrorTransport
		if ctx.Err() != nil {
			category = speech.ErrorCanceled
		}
		return nil, &speech.Error{Category: category, Cause: err,
			RemoteOutcomeUnknown: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBytes))
		category := speech.ErrorProvider
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			category = speech.ErrorAuthentication
		case http.StatusTooManyRequests:
			category = speech.ErrorRateLimited
		}
		return nil, &speech.Error{
			Category:             category,
			Status:               response.StatusCode,
			RemoteOutcomeUnknown: response.StatusCode >= 500,
		}
	}
	return response, nil
}
