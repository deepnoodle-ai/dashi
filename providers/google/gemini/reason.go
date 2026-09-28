package gemini

import (
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
)

// errorReason classifies Gemini failures. Gemini reports both context
// overflow and unusable media as 400 INVALID_ARGUMENT with no finer code, so
// the message decides: "The input token count (N) exceeds the maximum number of tokens
// allowed (M)." for context, and phrases such as "Unable to process input
// image" for media. Media phrases apply only when the request carried media,
// which is also why the request payload size limit is attributed to media.
func errorReason(r providerhttp.Rejection) llm.ErrorReason {
	if r.Type != "" && r.Type != "INVALID_ARGUMENT" {
		return ""
	}
	if r.Status != 0 && r.Status != 400 && r.Status != 413 {
		return ""
	}
	switch {
	case providerhttp.ContextWindowMessage(r.Message):
		return llm.ReasonContextWindowExceeded
	case r.HasMedia && providerhttp.ContainsAny(r.Message,
		"unable to process input image",
		"image is not valid",
		"unsupported mime type",
		"the document has no pages",
		"request payload size exceeds the limit"):
		return llm.ReasonMediaRejected
	}
	return ""
}
