package completions

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

// TestStreamErrorObjectFailsTheCall covers OpenAI-compatible servers that
// report a failure as an error object inside a successful HTTP stream.
// Dropping it either turned the failure into a finished result or into a
// transport error without the provider's explanation.
func TestStreamErrorObjectFailsTheCall(t *testing.T) {
	tests := []struct {
		name    string
		events  []string
		text    string
		message string
		want    llm.ErrorReason
	}{
		{
			// A context overflow reported inside the stream.
			name: "context window",
			events: []string{
				`{"error":{"message":"This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			},
			message: "This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.",
			want:    llm.ReasonContextWindowExceeded,
		},
		{
			// OpenRouter: an error object alongside finish_reason "error".
			name: "openrouter after text",
			events: []string{
				`{"id":"gen-1","model":"served","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
				`{"id":"gen-1","object":"chat.completion.chunk","error":{"code":"server_error","message":"Provider disconnected unexpectedly"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`,
			},
			text:    "Hel",
			message: "Provider disconnected unexpectedly",
		},
		{
			// A bare error object with no choices.
			name: "nested error",
			events: []string{
				`{"error":{"message":"The server had an error while processing your request.","type":"server_error","code":null}}`,
			},
			message: "The server had an error while processing your request.",
		},
		{
			// A bare string error.
			name:    "string error",
			events:  []string{`{"error":"upstream timed out"}`},
			message: "upstream timed out",
		},
		{
			// vLLM: a flat error object.
			name: "flat error",
			events: []string{
				`{"object":"error","message":"Internal server error","type":"InternalServerError","param":null,"code":500}`,
			},
			message: "Internal server error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range tt.events {
					io.WriteString(w, "data: "+event+"\n\n")
				}
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			p := testClient(t, server.URL)
			prepared, err := p.Prepare(t.Context(), testInput(true))
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Send(t.Context(), llm.Call{Request: prepared})
			e, ok := errors.AsType[*llm.Error](err)
			if !ok || e.Category != llm.ErrorProtocol ||
				!strings.Contains(string(e.Body), `error`) {
				t.Fatalf(
					"err = %v, want a protocol error carrying the provider body",
					err,
				)
			}
			if e.Reason != tt.want {
				t.Fatalf("reason = %q, want %q", e.Reason, tt.want)
			}
			if !strings.HasSuffix(err.Error(), "provider stream error: "+tt.message) {
				t.Fatalf("err = %v, want the provider's message %q", err, tt.message)
			}
			if result.Stop.Kind != llm.StopIncomplete ||
				result.Message.Text() != tt.text {
				t.Fatalf(
					"partial result = %+v, want incomplete with text %q",
					result,
					tt.text,
				)
			}
		})
	}
}
