package providers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/deepinfra"
	"github.com/deepnoodle-ai/kernel/providers/grok"
	"github.com/deepnoodle-ai/kernel/providers/meta"
	"github.com/deepnoodle-ai/kernel/providers/mistral"
	"github.com/deepnoodle-ai/kernel/providers/ollama"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/providers/openrouter"
)

// TestProviderRejectionsCarryReasons sends each provider a realistic
// rejection and checks the reason integrators act on, while the category
// that existing callers switch on stays unchanged.
func TestProviderRejectionsCarryReasons(t *testing.T) {
	must := func(c llm.Client, err error) llm.Client {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	clients := map[string]func(url string) llm.Client{
		"anthropic": func(url string) llm.Client {
			return must(messages.New(messages.Config{
				Provider:         "anthropic",
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"openai-responses": func(url string) llm.Client {
			return must(responses.New(responses.Config{
				Provider:         "openai",
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"openai-completions": func(url string) llm.Client {
			return must(completions.New(completions.Config{
				Provider:         "openai",
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"mistral": func(url string) llm.Client {
			return must(mistral.New(mistral.Config{
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"openrouter": func(url string) llm.Client {
			return must(openrouter.New(openrouter.Config{
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"deepinfra": func(url string) llm.Client {
			return must(deepinfra.New(deepinfra.Config{
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"grok": func(url string) llm.Client {
			return must(grok.New(grok.Config{
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"meta": func(url string) llm.Client {
			return must(meta.New(meta.Config{
				APIKey:           "k",
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
		"ollama": func(url string) llm.Client {
			return must(ollama.New(ollama.Config{
				Endpoint:         url,
				CapabilityPolicy: llm.CapabilityAdjust,
			}))
		},
	}
	// Bodies marked "recorded" were captured from live responses on
	// 2026-09-28 by TestLiveErrorReasons, with request IDs removed. The rest
	// are unverified reconstructions.
	tests := []struct {
		name, client string
		status       int
		body         string
		media        bool
		category     llm.ErrorCategory
		want         llm.ErrorReason
	}{
		{
			name:     "anthropic prompt too long (recorded)",
			client:   "anthropic",
			status:   400,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300023 tokens > 200000 maximum"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "anthropic corrupt image (recorded)",
			client:   "anthropic",
			status:   400,
			media:    true,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.1.image.source.base64.data: At least one of the image dimensions exceed max allowed size: 8000 pixels"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "anthropic unsupported image type (recorded)",
			client:   "anthropic",
			status:   400,
			media:    true,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.1.image.source.base64.media_type: Input should be 'image/jpeg', 'image/png', 'image/gif' or 'image/webp'"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "anthropic request too large with media",
			client:   "anthropic",
			status:   413,
			media:    true,
			body:     `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "anthropic request too large without media",
			client:   "anthropic",
			status:   413,
			body:     `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`,
			category: llm.ErrorInvalidRequest,
		},
		{
			name:     "anthropic overloaded",
			client:   "anthropic",
			status:   529,
			body:     `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			category: llm.ErrorOverloaded,
		},
		{
			name:     "openai responses context (recorded)",
			client:   "openai-responses",
			status:   400,
			body:     `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "openai responses corrupt image (recorded)",
			client:   "openai-responses",
			status:   400,
			media:    true,
			body:     `{"error":{"message":"The image data you provided does not represent a valid image. Please check your input and try again.","type":"invalid_request_error","param":"input","code":"invalid_value"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "openai responses unsupported image type (recorded)",
			client:   "openai-responses",
			status:   400,
			media:    true,
			body:     `{"error":{"message":"The image data you provided does not represent a valid image. Please check your input and try again with one of the supported image formats: ['image/jpeg', 'image/png', 'image/gif', 'image/webp'].","type":"invalid_request_error","param":"input","code":"invalid_value"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "openai responses invalid value without media",
			client:   "openai-responses",
			status:   400,
			body:     `{"error":{"message":"Invalid value: 'x'. Supported values are: 'auto'.","type":"invalid_request_error","param":"tool_choice","code":"invalid_value"}}`,
			category: llm.ErrorInvalidRequest,
		},
		{
			name:     "openai completions context (recorded)",
			client:   "openai-completions",
			status:   400,
			body:     `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 192007 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "openai completions corrupt image (recorded)",
			client:   "openai-completions",
			status:   400,
			media:    true,
			body:     `{"error":{"message":"You uploaded an unsupported image. Please make sure your image is valid.","type":"invalid_request_error","param":null,"code":"image_parse_error"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "openai completions unsupported image type (recorded)",
			client:   "openai-completions",
			status:   400,
			media:    true,
			body:     `{"error":{"message":"You uploaded an unsupported image. Please make sure your image has of one the following formats: ['png', 'jpeg', 'gif', 'webp'].","type":"invalid_request_error","param":null,"code":"invalid_image_format"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "openai tokens per minute",
			client:   "openai-completions",
			status:   429,
			body:     `{"error":{"message":"Request too large for gpt-4o in organization org-x on tokens per min (TPM): Limit 30000, Requested 50000.","type":"tokens","param":null,"code":"rate_limit_exceeded"}}`,
			category: llm.ErrorRateLimited,
		},
		{
			name:     "mistral context (recorded)",
			client:   "mistral",
			status:   400,
			body:     `{"object":"error","message":"Prompt 196611 > 131072 maximum context length","type":"invalid_request_prompt_too_long","param":null,"code":"3059","raw_status_code":400}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "openrouter context (recorded)",
			client:   "openrouter",
			status:   400,
			body:     `{"error":{"message":"This endpoint's maximum context length is 32768 tokens. However, you requested about 61456 tokens (61440 of text input, 16 in the output). Please reduce the length of either one, or use the context-compression plugin to compress your prompt automatically.","code":400,"metadata":{"provider_name":null}}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "deepinfra context (recorded)",
			client:   "deepinfra",
			status:   400,
			body:     `{"error":{"message":"Input too long: 196618 input tokens, limit is 131072 for this model","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "deepinfra too many images",
			client:   "deepinfra",
			status:   400,
			media:    true,
			body:     `{"error":{"message":"At most 1 image(s) may be provided in one request."}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "grok corrupt image (recorded)",
			client:   "grok",
			status:   400,
			media:    true,
			body:     `{"code":"invalid_image","error":"code: 'Client specified an invalid argument', message: \"Invalid PNG image."}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "grok unsupported image type (recorded)",
			client:   "grok",
			status:   400,
			media:    true,
			body:     `{"code":"invalid_image","error":"code: 'Client specified an invalid argument', message: \"Downloaded response does not contain a valid JPG, PNG, WebP, or ICO image."}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonMediaRejected,
		},
		{
			name:     "grok context",
			client:   "grok",
			status:   400,
			body:     `{"code":"Client specified an invalid argument","error":"This model's maximum prompt length is 131072 but the request contains 136157 tokens."}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "meta context",
			client:   "meta",
			status:   400,
			body:     `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error"}}`,
			category: llm.ErrorInvalidRequest,
			want:     llm.ReasonContextWindowExceeded,
		},
		{
			name:     "ollama unclassified",
			client:   "ollama",
			status:   400,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"model is required"}}`,
			category: llm.ErrorInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(
				t,
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					io.WriteString(w, tt.body)
				},
			)
			p := clients[tt.client](server.URL)
			input := modelInput()
			if tt.media {
				input.Messages[0].Content = append(
					input.Messages[0].Content,
					llm.Block(content.ImageBlock(content.Source{
						Data:     []byte{1, 2},
						MIMEType: "image/png",
					})),
				)
			}
			prepared, err := p.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Send(context.Background(), llm.Call{Request: prepared})
			var e *llm.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *llm.Error", err)
			}
			if e.Category != tt.category || e.Reason != tt.want {
				t.Fatalf(
					"category, reason = %s, %q; want %s, %q",
					e.Category,
					e.Reason,
					tt.category,
					tt.want,
				)
			}
			if llm.IsContextWindowExceeded(err) != (tt.want == llm.ReasonContextWindowExceeded) ||
				llm.IsMediaRejected(err) != (tt.want == llm.ReasonMediaRejected) {
				t.Fatalf("helpers disagree with reason %q", e.Reason)
			}
		})
	}
}

// TestInStreamRejectionsCarryReasons covers failures reported after a
// successful HTTP response, inside the event stream.
func TestInStreamRejectionsCarryReasons(t *testing.T) {
	tests := []struct {
		name   string
		client func(url string) llm.Client
		stream string
		media  bool
		want   llm.ErrorReason
	}{
		{
			name: "anthropic image error event with media",
			client: func(url string) llm.Client {
				return newMessages(
					t,
					messages.Config{
						Provider:         "anthropic",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"}}`),
			media:  true,
			want:   llm.ReasonMediaRejected,
		},
		{
			name: "anthropic image error event without media",
			client: func(url string) llm.Client {
				return newMessages(
					t,
					messages.Config{
						Provider:         "anthropic",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"}}`),
		},
		{
			name: "responses image error event with media",
			client: func(url string) llm.Client {
				return newResponses(
					t,
					responses.Config{
						Provider:         "openai",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","code":"invalid_value","message":"The image data you provided does not represent a valid image.","param":"input","sequence_number":1}`),
			media:  true,
			want:   llm.ReasonMediaRejected,
		},
		{
			name: "responses image error event without media",
			client: func(url string) llm.Client {
				return newResponses(
					t,
					responses.Config{
						Provider:         "openai",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","code":"invalid_value","message":"The image data you provided does not represent a valid image.","param":"input","sequence_number":1}`),
		},
		{
			name: "anthropic error event",
			client: func(url string) llm.Client {
				return newMessages(
					t,
					messages.Config{
						Provider:         "anthropic",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`),
			want:   llm.ReasonContextWindowExceeded,
		},
		{
			name: "anthropic overloaded event",
			client: func(url string) llm.Client {
				return newMessages(
					t,
					messages.Config{
						Provider:         "anthropic",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
		},
		{
			name: "responses error event",
			client: func(url string) llm.Client {
				return newResponses(
					t,
					responses.Config{
						Provider:         "openai",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model.","param":null,"sequence_number":1}`),
			want:   llm.ReasonContextWindowExceeded,
		},
		{
			name: "responses failed response",
			client: func(url string) llm.Client {
				return newResponses(
					t,
					responses.Config{
						Provider:         "openai",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"response.failed","response":{"object":"response","id":"r","model":"served","status":"failed","output":[],"error":{"code":"invalid_base64_image","message":"Invalid base64 image data."}}}`),
			want:   llm.ReasonMediaRejected,
		},
		{
			name: "responses server error",
			client: func(url string) llm.Client {
				return newResponses(
					t,
					responses.Config{
						Provider:         "openai",
						APIKey:           "k",
						Endpoint:         url,
						CapabilityPolicy: llm.CapabilityAdjust,
					},
				)
			},
			stream: events(`{"type":"response.failed","response":{"object":"response","id":"r","model":"served","status":"failed","output":[],"error":{"code":"server_error","message":"The server had an error."}}}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(
				t,
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, tt.stream)
				},
			)
			p := tt.client(server.URL)
			input := modelInput()
			input.Stream = true
			if tt.media {
				input.Messages[0].Content = append(
					input.Messages[0].Content,
					llm.Block(content.ImageBlock(content.Source{
						Data:     []byte{1, 2},
						MIMEType: "image/png",
					})),
				)
			}
			prepared, err := p.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Send(
				context.Background(),
				llm.Call{
					Request: prepared,
					Sink: func(llm.Delta) {

					},
				},
			)
			var e *llm.Error
			if !errors.As(
				err,
				&e,
			) || e.Category != llm.ErrorProtocol || e.Reason != tt.want {
				t.Fatalf("err = %v, want protocol error with reason %q", err, tt.want)
			}
		})
	}
}
