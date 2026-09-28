package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/model"
	"github.com/deepnoodle-ai/dashi/speech"
)

var errResponseTooLarge = errors.New("speech response exceeds size limit")

// SendTranscription makes one upload attempt and returns a completed result.
// A transport failure can leave the remote outcome unknown.
func (c *Client) SendTranscription(ctx context.Context,
	request speech.TranscriptionRequest) (speech.Transcription, error) {
	r, ok := request.(*preparedTranscription)
	if !ok || r == nil || r.client != c {
		return speech.Transcription{}, fail(speech.ErrorInvalidRequest,
			errors.New("prepared transcription belongs to another client"))
	}
	if err := ctx.Err(); err != nil {
		return speech.Transcription{}, fail(speech.ErrorCanceled, err)
	}
	if c.apiKey == "" {
		return speech.Transcription{}, fail(speech.ErrorAuthentication,
			errors.New("OPENAI_API_KEY or Config.APIKey is required"))
	}
	response, err := c.do(ctx, "/audio/transcriptions", r.ctype, r.body)
	if err != nil {
		return speech.Transcription{}, err
	}
	defer response.Body.Close()
	if ctype := response.Header.Get("Content-Type"); ctype != "" {
		mediaType, _, parseErr := mime.ParseMediaType(ctype)
		if parseErr != nil || mediaType != "application/json" {
			return speech.Transcription{}, &speech.Error{
				Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
				Cause: errors.New("transcription response is not JSON"),
			}
		}
	}
	body, err := readBounded(response.Body, maxJSONBytes)
	if err != nil {
		return speech.Transcription{}, responseError(ctx, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return speech.Transcription{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
			Cause: errors.New("invalid transcription JSON object"),
		}
	}
	textRaw, exists := fields["text"]
	if !exists || bytes.Equal(bytes.TrimSpace(textRaw), []byte("null")) {
		return speech.Transcription{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
			Cause: errors.New("transcription response has no text string"),
		}
	}
	var resultText string
	if err := json.Unmarshal(textRaw, &resultText); err != nil {
		return speech.Transcription{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
			Cause: errors.New("transcription text is not a string"),
		}
	}
	result := speech.Transcription{
		Text:      resultText,
		Requested: model.ModelRef{Provider: "openai", Model: transcribeModel},
		API:       TranscriptionsAPI,
	}
	if err := result.Validate(); err != nil {
		return speech.Transcription{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true, Cause: err,
		}
	}
	return result, nil
}

// SendSynthesis makes one speech creation attempt and returns inline MP3 data.
func (c *Client) SendSynthesis(ctx context.Context,
	request speech.SynthesisRequest) (speech.Synthesis, error) {
	r, ok := request.(*preparedSynthesis)
	if !ok || r == nil || r.client != c {
		return speech.Synthesis{}, fail(speech.ErrorInvalidRequest,
			errors.New("prepared synthesis belongs to another client"))
	}
	if err := ctx.Err(); err != nil {
		return speech.Synthesis{}, fail(speech.ErrorCanceled, err)
	}
	if c.apiKey == "" {
		return speech.Synthesis{}, fail(speech.ErrorAuthentication,
			errors.New("OPENAI_API_KEY or Config.APIKey is required"))
	}
	response, err := c.do(ctx, "/audio/speech", "application/json", r.body)
	if err != nil {
		return speech.Synthesis{}, err
	}
	defer response.Body.Close()
	if ctype := response.Header.Get("Content-Type"); ctype != "" {
		mediaType, _, parseErr := mime.ParseMediaType(ctype)
		if parseErr != nil || (mediaType != "audio/mpeg" &&
			mediaType != "application/octet-stream") {
			return speech.Synthesis{}, &speech.Error{
				Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
				Cause: errors.New("speech response media type conflicts with MP3"),
			}
		}
	}
	body, err := readBounded(response.Body, maxResponseBytes)
	if err != nil {
		return speech.Synthesis{}, responseError(ctx, err)
	}
	if !hasCompleteMP3Frame(body) {
		return speech.Synthesis{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true,
			Cause: errors.New("speech response has no complete MP3 frame"),
		}
	}
	result := speech.Synthesis{
		Audio: content.AudioBlock(content.Source{
			Data: body, MIMEType: "audio/mpeg",
		}),
		Requested: model.ModelRef{Provider: "openai", Model: synthesisModel},
		API:       SpeechAPI,
	}
	if err := result.Validate(); err != nil {
		return speech.Synthesis{}, &speech.Error{
			Category: speech.ErrorProtocol, RemoteOutcomeUnknown: true, Cause: err,
		}
	}
	return result, nil
}

func readBounded(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func responseError(ctx context.Context, err error) error {
	category := speech.ErrorProtocol
	if ctx.Err() != nil {
		category = speech.ErrorCanceled
	} else if !errors.Is(err, errResponseTooLarge) {
		category = speech.ErrorTransport
	}
	return &speech.Error{
		Category: category, RemoteOutcomeUnknown: true, Cause: err,
	}
}
