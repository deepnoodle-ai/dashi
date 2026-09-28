package providers_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// These adapters must preserve the same agent-facing final facts in
// blocking and streaming mode. Wire fixtures use each provider's own format.
// The Gemini fixture lives in the providers/google/gemini module.
func TestAgentFinalFactsAgreeAcrossProviderTransports(t *testing.T) {
	type fixture struct {
		name string
		providertest.TransportFixture
	}
	fixtures := []fixture{
		{
			name: "anthropic",
			TransportFixture: providertest.TransportFixture{
				Complete: `{"id":"r1","type":"message","model":"served","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`,
				Stream: events(
					`{"type":"message_start","message":{"id":"r1","type":"message","model":"served","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
					`{"type":"message_stop"}`,
				),
				New: func(url string) llm.Client {
					return newMessages(
						t,
						messages.Config{
							Provider:         "anthropic",
							APIKey:           "test",
							Endpoint:         url,
							CapabilityPolicy: llm.CapabilityAdjust,
						},
					)
				},
			},
		},
		{
			name: "openai",
			TransportFixture: providertest.TransportFixture{
				Complete: `{"id":"r1","object":"response","model":"served","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`,
				Stream: events(
					`{"type":"response.created","response":{"id":"r1","object":"response","model":"served","status":"in_progress","output":[]}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
					`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
					`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
					`{"type":"response.completed","response":{"id":"r1","object":"response","model":"served","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
				),
				New: func(url string) llm.Client {
					return newResponses(
						t,
						responses.Config{
							Provider:         "openai",
							APIKey:           "test",
							Endpoint:         url,
							CapabilityPolicy: llm.CapabilityAdjust,
						},
					)
				},
			},
		},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			providertest.FinalFactsAgree(t, tc.TransportFixture)
		})
	}
}

func TestIteratorBreakCancelsProviderHTTPRequest(t *testing.T) {
	canceled := make(chan bool, 1)
	release := make(chan struct{})
	defer close(release)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			events(
				`{"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress","output":[]}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message","role":"assistant","content":[]}}`,
				`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
				`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"first"}`,
			),
		)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			canceled <- true
		case <-release:
			canceled <- false
		}
	})
	client := newResponses(
		t,
		responses.Config{
			Provider:         "openai",
			APIKey:           "test",
			Endpoint:         server.URL,
			CapabilityPolicy: llm.CapabilityAdjust,
		},
	)
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "requested",
			Stream:   true,
			Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for event, err := range llm.Stream(
		t.Context(),
		client,
		llm.Call{
			Request: request,
		},
	) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Delta != nil && event.Delta.Text == "first" {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("stream did not yield text before cancellation")
	}
	select {
	case ok := <-canceled:
		if !ok {
			t.Fatal("server request ended without client cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider request stayed open after iterator break")
	}
}
