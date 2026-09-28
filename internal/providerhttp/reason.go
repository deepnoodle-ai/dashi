package providerhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

// Rejection is a provider failure reduced to the fields a Classifier reads.
type Rejection struct {
	// Status is the HTTP status, or zero for an error reported in a stream or
	// response body.
	Status int
	// Code is the provider's string error code, if any.
	Code string
	// Type is the provider's error type or status name, such as
	// "invalid_request_error" or "INVALID_ARGUMENT".
	Type string
	// Message is the provider's human-readable message.
	Message string
	// HasMedia reports whether the request carried image, document, audio, or
	// video content. It is false when unknown.
	HasMedia bool
}

// Classifier maps a provider failure to an llm.ErrorReason. It returns the
// empty reason when the failure is not recognized. Classifiers must prefer
// leaving a failure unclassified over guessing: a false reason sends the
// caller down the wrong recovery path.
type Classifier func(Rejection) llm.ErrorReason

// ParseRejection decodes the common JSON error envelopes: the nested
// {"error":{"code","type","status","message"}} shape, a flat
// {"code","type","message"} shape, a bare {"error":"message"}, a
// {"detail":"message"}, and a one-element array of any of these. Numeric codes
// are ignored because they repeat the HTTP status. A body that is not JSON
// becomes the message.
func ParseRejection(status int, body []byte) Rejection {
	r := Rejection{Status: status}
	var list []json.RawMessage
	if json.Unmarshal(body, &list) == nil && len(list) > 0 {
		body = list[0]
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		r.Message = strings.TrimSpace(string(body))
		return r
	}
	read := func(fields map[string]json.RawMessage) {
		str := func(key string) string {
			var s string
			_ = json.Unmarshal(fields[key], &s)
			return s
		}
		r.Code, r.Message = str("code"), str("message")
		if r.Type = str("type"); r.Type == "" {
			r.Type = str("status")
		}
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(envelope["error"], &nested) == nil && nested != nil {
		read(nested)
		return r
	}
	read(envelope)
	if r.Type == "error" {
		// A stream event's own type, not an error type.
		r.Type = ""
	}
	if r.Message == "" {
		var s string
		if json.Unmarshal(envelope["error"], &s) == nil {
			r.Message = s
		} else if json.Unmarshal(envelope["detail"], &s) == nil {
			r.Message = s
		}
	}
	return r
}

// Classify sets the reason on err's *llm.Error from its Body when the error
// has no reason yet. It serves errors reported inside a stream or response
// body; hasMedia reports whether the request carried media. It returns err.
func Classify(err error, classify Classifier, hasMedia bool) error {
	var e *llm.Error
	if !errors.As(err, &e) || e.Reason != "" || len(e.Body) == 0 {
		return err
	}
	r := ParseRejection(e.Status, e.Body)
	r.HasMedia = hasMedia
	e.Reason = Reason(classify, r)
	return err
}

// Reason classifies a rejection with the provider's classifier, which may be
// nil, and then the provider-independent rules. A 413 on a request that
// carries media is attributed to the media when the provider's classifier
// does not decide: media is by far the usual way to exceed a provider's byte
// limit, and it is the part a caller can shrink or drop.
func Reason(classify Classifier, r Rejection) llm.ErrorReason {
	if classify != nil {
		if reason := classify(r); reason != "" {
			return reason
		}
	}
	if r.Status == http.StatusRequestEntityTooLarge && r.HasMedia {
		return llm.ReasonMediaRejected
	}
	return ""
}

// HasMedia reports whether input carries image, document, audio, or video
// content, including inside tool results.
func HasMedia(input llm.Input) bool {
	media := func(blocks ...content.Block) bool {
		for _, b := range blocks {
			switch b.Kind {
			case content.KindImage, content.KindDocument, content.KindAudio, content.KindVideo:
				return true
			}
		}
		return false
	}
	for _, m := range input.Messages {
		for _, c := range m.Content {
			if c.Block != nil && media(*c.Block) {
				return true
			}
			if c.ToolResult != nil &&
				media(c.ToolResult.Output.Content...) {
				return true
			}
		}
	}
	return false
}

// contextWindowPhrases are lowercase message fragments that, across providers
// and OpenAI-compatible servers, report only an input that exceeds the model's
// context window. Each is specific enough not to match per-minute token
// limits ("Request too large ... tokens per min") or output limits
// ("max_tokens is too large"). Sources marked "live" were recorded from
// provider responses (docs/dev/live-tests.md); the rest are unverified.
var contextWindowPhrases = []string{
	"context_length_exceeded",
	"maximum context length",             // live: OpenAI Chat, OpenRouter, Mistral; vLLM
	"exceeds the context window",         // live: OpenAI Responses
	"prompt is too long",                 // live: Anthropic
	"exceed context limit",               // Anthropic input plus max_tokens, older models
	"maximum prompt length is",           // xAI
	"exceeds the available context size", // llama.cpp servers
}

// ContextWindowMessage reports whether a provider message states that the
// input exceeded the model's context window.
func ContextWindowMessage(message string) bool {
	// Gemini: "The input token count (N) exceeds the maximum number of tokens
	// allowed (M)."
	return ContainsAny(message, contextWindowPhrases...) ||
		ContainsAny(
			message,
			"input token count",
		) && ContainsAny(
			message,
			"exceeds the maximum number of tokens",
		)
}

// ContainsAny reports whether message, ignoring case, contains any of the
// lowercase phrases.
func ContainsAny(message string, phrases ...string) bool {
	m := strings.ToLower(message)
	for _, phrase := range phrases {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// rejectsRequest reports whether r can describe a problem with the request
// itself. Rate limits and server failures never do, whatever their message.
func (r Rejection) rejectsRequest() bool {
	return r.Status == 0 ||
		(r.Status >= 400 &&
			r.Status < 500 &&
			r.Status != http.StatusTooManyRequests)
}

// openAIMediaCodes are the error codes OpenAI reports for image input it cannot
// use. Content policy rejections are excluded: they concern what an image
// shows, not whether the provider can accept it.
var openAIMediaCodes = map[string]bool{
	"invalid_image":                true,
	"invalid_image_format":         true,
	"invalid_base64_image":         true,
	"invalid_image_url":            true,
	"invalid_image_mode":           true,
	"image_too_large":              true,
	"image_too_small":              true,
	"image_file_too_large":         true,
	"image_parse_error":            true,
	"unsupported_image_media_type": true,
	"empty_image_file":             true,
	"failed_to_download_image":     true,
	"image_file_not_found":         true,
}

// OpenAIReason classifies failures in the error envelope shared by OpenAI's
// Chat Completions and Responses APIs and by OpenAI-compatible servers. The
// structured code decides when present; message phrases cover compatible
// servers that send no code, and media phrases apply only when the request
// carried media.
func OpenAIReason(r Rejection) llm.ErrorReason {
	switch {
	case !r.rejectsRequest():
		return ""
	case r.Code == "context_length_exceeded", // OpenAI, DeepInfra
		r.Type == "invalid_request_prompt_too_long": // Mistral
		return llm.ReasonContextWindowExceeded
	case openAIMediaCodes[r.Code]: // Chat Completions, xAI
		return llm.ReasonMediaRejected
	case ContextWindowMessage(r.Message):
		return llm.ReasonContextWindowExceeded
	case r.HasMedia && ContainsAny(
		r.Message,
		// OpenAI Responses reports unusable images as code invalid_value.
		"does not represent a valid image",
		"too many images",
		"image(s) may be provided", // vLLM
		"invalid image",
		"unsupported image",
		"could not process image",
		"failed to load image",
	):
		return llm.ReasonMediaRejected
	}
	return ""
}
