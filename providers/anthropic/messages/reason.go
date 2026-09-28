package messages

import (
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
)

// errorReason classifies Anthropic Messages failures, which carry an error
// type but no code. Context overflow is reported as invalid_request_error
// with "prompt is too long: N tokens > M maximum" (older models also rejected
// input plus max_tokens with "input length and `max_tokens` exceed context
// limit"). Media validation failures name the offending field path; a
// corrupt PNG, for example, is reported as
// "messages.0.content.1.image.source.base64.data: At least one of the image
// dimensions exceed max allowed size: 8000 pixels".
func errorReason(r providerhttp.Rejection) llm.ErrorReason {
	if r.Type != "" &&
		r.Type != "invalid_request_error" &&
		r.Type != "request_too_large" {
		return ""
	}
	if r.Status != 0 && (r.Status < 400 || r.Status >= 500 || r.Status == 429) {
		return ""
	}
	switch {
	case providerhttp.ContextWindowMessage(r.Message):
		return llm.ReasonContextWindowExceeded
	case providerhttp.ContainsAny(r.Message, ".image.source", ".document.source"):
		return llm.ReasonMediaRejected
	case r.HasMedia && providerhttp.ContainsAny(
		r.Message,
		"could not process image",
		"image exceeds",
		"image dimensions exceed",
		"too many images",
		"pdf specified was not valid",
		"pdf pages",
	):
		return llm.ReasonMediaRejected
	}
	return ""
}
