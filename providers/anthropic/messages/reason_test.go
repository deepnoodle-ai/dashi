package messages

import (
	"testing"

	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestErrorReason(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		media  bool
		want   llm.ErrorReason
	}{
		// Recorded live on 2026-09-28; request ID removed.
		{name: "prompt too long", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300023 tokens > 200000 maximum"}}`,
			want: llm.ReasonContextWindowExceeded},
		// Recorded live on 2026-09-28 for a PNG signature followed by junk.
		{name: "corrupt png", status: 400, media: true,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.1.image.source.base64.data: At least one of the image dimensions exceed max allowed size: 8000 pixels"}}`,
			want: llm.ReasonMediaRejected},
		{name: "input and max tokens", status: 400,
			body: "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"input length and `max_tokens` exceed context limit: 188240 + 20000 > 200000, decrease input length or `max_tokens` and try again\"}}",
			want: llm.ReasonContextWindowExceeded},
		{name: "stream event", body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`,
			want: llm.ReasonContextWindowExceeded},
		{name: "image size path", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.1.image.source.base64: image exceeds 5 MB maximum: 7340032 bytes > 5242880 bytes"}}`,
			want: llm.ReasonMediaRejected},
		{name: "image media type path", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64.media_type: Input should be 'image/jpeg', 'image/png', 'image/gif' or 'image/webp'"}}`,
			want: llm.ReasonMediaRejected},
		{name: "invalid pdf path", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.document.source.base64.data: The PDF specified was not valid."}}`,
			want: llm.ReasonMediaRejected},
		{name: "unprocessable image", status: 400, media: true,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"}}`,
			want: llm.ReasonMediaRejected},
		{name: "unprocessable image without media", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"}}`},
		{name: "max tokens too large", status: 400,
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 100000 > 64000, which is the maximum allowed number of output tokens for claude-sonnet"}}`},
		{name: "rate limit", status: 429,
			body: `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed the rate limit for your organization of 30,000 input tokens per minute."}}`},
		{name: "overloaded", status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		{name: "request too large leaves media to the shared rule", status: 413, media: true,
			body: `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := providerhttp.ParseRejection(tt.status, []byte(tt.body))
			r.HasMedia = tt.media
			if got := errorReason(r); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}
