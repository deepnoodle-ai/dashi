package providers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

// All three adapters must preserve the same agent-facing final facts in
// blocking and streaming mode. Wire fixtures use each provider's own format.
func TestAgentFinalFactsAgreeAcrossProviderTransports(t *testing.T) {
	type fixture struct {
		name     string
		complete string
		stream   string
		new      func(string) llm.Client
	}
	fixtures := []fixture{
		{
			name:     "anthropic",
			complete: `{"id":"r1","type":"message","model":"served","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`,
			stream: events(
				`{"type":"message_start","message":{"id":"r1","type":"message","model":"served","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
				`{"type":"message_stop"}`),
			new: func(url string) llm.Client {
				return newMessages(t, messages.Config{Provider: "anthropic", APIKey: "test", Endpoint: url, CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		{
			name:     "openai",
			complete: `{"id":"r1","object":"response","model":"served","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`,
			stream: events(
				`{"type":"response.created","response":{"id":"r1","object":"response","model":"served","status":"in_progress","output":[]}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
				`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
				`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
				`{"type":"response.completed","response":{"id":"r1","object":"response","model":"served","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`),
			new: func(url string) llm.Client {
				return newResponses(t, responses.Config{Provider: "openai", APIKey: "test", Endpoint: url, CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		{
			name:     "gemini",
			complete: `{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
			stream:   events(`{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`),
			new: func(url string) llm.Client {
				client, err := gemini.New(gemini.Config{Provider: "google", APIKey: "test", Endpoint: url + "/v1/models/{model}:generateContent", CapabilityPolicy: llm.CapabilityAdjust})
				if err != nil {
					t.Fatal(err)
				}
				return client
			},
		},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				stream := strings.Contains(r.URL.Path, "streamGenerateContent") || r.URL.Query().Get("alt") == "sse"
				if !stream {
					var body struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("request body: %v", err)
					}
					stream = body.Stream
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.stream)
				} else {
					_, _ = io.WriteString(w, tc.complete)
				}
			})
			client := tc.new(server.URL)
			for _, stream := range []bool{false, true} {
				input := llm.Input{Model: "requested", Stream: stream, Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}}
				request, err := client.Prepare(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
				var result llm.Result
				if stream {
					deltas, terminals := 0, 0
					for event, err := range llm.Stream(context.Background(), client, request) {
						if err != nil {
							t.Fatal(err)
						}
						if event.Delta != nil {
							deltas++
						}
						if event.Result != nil {
							terminals++
							result = *event.Result
						}
					}
					if deltas == 0 || terminals != 1 {
						t.Fatalf("stream events: deltas=%d terminals=%d", deltas, terminals)
					}
				} else {
					result, err = client.Send(context.Background(), request, nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				if result.Stop.Kind != llm.StopFinished || result.Message.Text() != "Hello" || result.Usage == nil || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 2 || result.ID != "r1" {
					t.Fatalf("stream=%t result=%+v", stream, result)
				}
			}
		})
	}
}

func TestIteratorBreakCancelsProviderHTTPRequest(t *testing.T) {
	canceled := make(chan bool, 1)
	release := make(chan struct{})
	defer close(release)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, events(
			`{"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"first"}`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			canceled <- true
		case <-release:
			canceled <- false
		}
	})
	client := newResponses(t, responses.Config{Provider: "openai", APIKey: "test", Endpoint: server.URL, CapabilityPolicy: llm.CapabilityAdjust})
	request, err := client.Prepare(context.Background(), llm.Input{Model: "requested", Stream: true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for event, err := range llm.Stream(context.Background(), client, request) {
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
