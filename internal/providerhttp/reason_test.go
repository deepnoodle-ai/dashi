package providerhttp

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestParseRejectionReadsCommonEnvelopes(t *testing.T) {
	tests := []struct {
		name, body string
		want       Rejection
	}{
		{name: "nested", body: `{"error":{"message":"m","type":"invalid_request_error","code":"c"}}`,
			want: Rejection{Code: "c", Type: "invalid_request_error", Message: "m"}},
		{name: "google status", body: `{"error":{"code":400,"message":"m","status":"INVALID_ARGUMENT"}}`,
			want: Rejection{Type: "INVALID_ARGUMENT", Message: "m"}},
		{name: "google array", body: `[{"error":{"code":400,"message":"m","status":"INVALID_ARGUMENT"}}]`,
			want: Rejection{Type: "INVALID_ARGUMENT", Message: "m"}},
		{name: "flat", body: `{"object":"error","message":"m","type":"invalid_request_error","code":null}`,
			want: Rejection{Type: "invalid_request_error", Message: "m"}},
		{name: "stream event", body: `{"type":"error","code":"c","message":"m"}`,
			want: Rejection{Code: "c", Message: "m"}},
		{name: "string error", body: `{"code":"c","error":"m"}`, want: Rejection{Code: "c", Message: "m"}},
		{name: "detail", body: `{"detail":"m"}`, want: Rejection{Message: "m"}},
		{name: "plain", body: " m\n", want: Rejection{Message: "m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.want.Status = 400
			if got := ParseRejection(400, []byte(tt.body)); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestOpenAIReason(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		media  bool
		want   llm.ErrorReason
	}{
		{name: "context code", status: 400,
			body: `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			want: llm.ReasonContextWindowExceeded},
		{name: "context code in stream", body: `{"type":"error","code":"context_length_exceeded","message":"too long"}`,
			want: llm.ReasonContextWindowExceeded},
		{name: "vllm message", status: 400,
			body: `{"object":"error","message":"This model's maximum context length is 8192 tokens. However, you requested 9000 tokens (8000 in the messages, 1000 in the completion).","type":"BadRequestError","code":400}`,
			want: llm.ReasonContextWindowExceeded},
		{name: "llama.cpp message", status: 400,
			body: `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error"}}`,
			want: llm.ReasonContextWindowExceeded},
		// Recorded live on 2026-09-28 (OpenAI Responses, corrupt PNG).
		{name: "responses invalid value image", status: 400, media: true,
			body: `{"error":{"message":"The image data you provided does not represent a valid image. Please check your input and try again.","type":"invalid_request_error","param":"input","code":"invalid_value"}}`,
			want: llm.ReasonMediaRejected},
		{name: "invalid value without media", status: 400,
			body: `{"error":{"message":"The image data you provided does not represent a valid image.","type":"invalid_request_error","param":"input","code":"invalid_value"}}`},
		// Recorded live on 2026-09-28 (Mistral).
		{name: "mistral prompt too long", status: 400,
			body: `{"object":"error","message":"Prompt 196611 > 131072 maximum context length","type":"invalid_request_prompt_too_long","param":null,"code":"3059","raw_status_code":400}`,
			want: llm.ReasonContextWindowExceeded},
		{name: "image code without media flag", status: 400,
			body: `{"error":{"message":"You uploaded an unsupported image.","type":"invalid_request_error","code":"invalid_image_format"}}`,
			want: llm.ReasonMediaRejected},
		{name: "image download", status: 400,
			body: `{"error":{"message":"Error while downloading https://example.com/a.png.","type":"invalid_request_error","code":"invalid_image_url"}}`,
			want: llm.ReasonMediaRejected},
		{name: "image phrase with media", status: 400, media: true,
			body: `{"error":{"message":"Too many images in request. Max is 500."}}`,
			want: llm.ReasonMediaRejected},
		{name: "image phrase without media", status: 400,
			body: `{"error":{"message":"Too many images in request. Max is 500."}}`},
		{name: "content policy", status: 400, media: true,
			body: `{"error":{"message":"Your image was flagged.","code":"image_content_policy_violation"}}`},
		{name: "tokens per minute", status: 429,
			body: `{"error":{"message":"Request too large for gpt-4o on tokens per min (TPM): Limit 30000, Requested 50000.","code":"rate_limit_exceeded"}}`},
		{name: "rate limit mentioning context", status: 429,
			body: `{"error":{"message":"maximum context length is 8192 tokens","code":"rate_limit_exceeded"}}`},
		{name: "server error", status: 500, body: `{"error":{"message":"maximum context length exceeded"}}`},
		{name: "max tokens", status: 400,
			body: `{"error":{"message":"max_tokens is too large: 50000. This model supports at most 16384 completion tokens.","type":"invalid_request_error","code":"invalid_value"}}`},
		{name: "generic", status: 400, body: `{"error":{"message":"Invalid 'messages[0].role'.","code":"invalid_value"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ParseRejection(tt.status, []byte(tt.body))
			r.HasMedia = tt.media
			if got := OpenAIReason(r); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReasonAttributesOversizedMediaRequests(t *testing.T) {
	tests := []struct {
		name     string
		r        Rejection
		classify Classifier
		want     llm.ErrorReason
	}{
		{name: "413 with media", r: Rejection{Status: 413, HasMedia: true}, want: llm.ReasonMediaRejected},
		{name: "413 without media", r: Rejection{Status: 413}},
		{name: "400 with media", r: Rejection{Status: 400, HasMedia: true}},
		{name: "classifier decides", r: Rejection{Status: 413, HasMedia: true},
			classify: func(Rejection) llm.ErrorReason { return llm.ReasonContextWindowExceeded },
			want:     llm.ReasonContextWindowExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reason(tt.classify, tt.r); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyKeepsExistingReason(t *testing.T) {
	always := func(Rejection) llm.ErrorReason { return llm.ReasonMediaRejected }
	set := &llm.Error{Reason: llm.ReasonContextWindowExceeded, Body: []byte(`{}`)}
	if Classify(set, always); set.Reason != llm.ReasonContextWindowExceeded {
		t.Fatal("Classify replaced an existing reason")
	}
	empty := &llm.Error{}
	if Classify(empty, always); empty.Reason != "" {
		t.Fatal("Classify reasoned without a body")
	}
	if err := Classify(errors.New("plain"), always); err.Error() != "plain" {
		t.Fatal("Classify changed a non-llm error")
	}
}

func TestHasMediaFindsNestedMedia(t *testing.T) {
	image := content.ImageBlock(content.Source{Data: []byte{1}, MIMEType: "image/png"})
	text := llm.Input{Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}}
	if HasMedia(text) {
		t.Fatal("text input reported media")
	}
	direct := llm.Input{Messages: []llm.Message{llm.UserMessage(llm.Text("hi"), llm.Block(image))}}
	nested := llm.Input{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Content{{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: "c", Output: tool.Output{Content: []content.Block{image}}},
	}}}}}
	if !HasMedia(direct) || !HasMedia(nested) {
		t.Fatal("media input was not detected")
	}
}
