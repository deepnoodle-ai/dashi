package providers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

type staticTransport func(*http.Request) (*http.Response, error)

func (f staticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestHTTPModelAdaptersReportIdleTimeoutWithPartialResult(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		new    func(string) (llm.Client, error)
	}{
		{
			name: "responses",
			prefix: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"model\":\"m\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
				"data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hi\"}\n\n",
			new: func(endpoint string) (llm.Client, error) {
				return responses.New(responses.Config{
					Provider: "test", APIKey: "key", Endpoint: endpoint,
					Timeouts: llm.HTTPTimeouts{StreamIdle: 80 * time.Millisecond},
				})
			},
		},
		{
			name:   "completions",
			prefix: "data: {\"id\":\"chat-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
			new: func(endpoint string) (llm.Client, error) {
				return completions.New(completions.Config{
					Provider: "test", APIKey: "key", Endpoint: endpoint,
					Timeouts: llm.HTTPTimeouts{StreamIdle: 80 * time.Millisecond},
				})
			},
		},
		{
			name: "messages",
			prefix: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"type\":\"message\",\"model\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			new: func(endpoint string) (llm.Client, error) {
				return messages.New(messages.Config{
					Provider: "test", APIKey: "key", Endpoint: endpoint,
					Timeouts: llm.HTTPTimeouts{StreamIdle: 80 * time.Millisecond},
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.prefix)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			client, err := tc.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			prepared, err := client.Prepare(ctx, llm.Input{
				Model: "m", Stream: true,
				Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Send(ctx, llm.Call{Request: prepared})
			failure, ok := errors.AsType[*llm.Error](err)
			if !ok || failure.Category != llm.ErrorTransport ||
				failure.Reason != llm.ReasonStreamIdleTimeout ||
				!errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
			if result.Message.Text() != "hi" || result.Stop.Kind != llm.StopIncomplete {
				t.Fatalf("partial result=%+v", result)
			}
		})
	}
}

func TestSuccessfulStreamSurvivesLateTimeoutTimer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	total := 80 * time.Millisecond
	client, err := completions.New(completions.Config{
		Provider: "test", APIKey: "key", Endpoint: server.URL,
		Timeouts: llm.HTTPTimeouts{
			ResponseStart: total, StreamIdle: total, Total: &total,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.Prepare(t.Context(), llm.Input{
		Model: "m", Stream: true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Send(t.Context(), llm.Call{Request: prepared})
	if err != nil || result.Stop.Kind != llm.StopFinished ||
		result.Message.Text() != "done" {
		t.Fatalf("terminal result=%+v err=%v", result, err)
	}
	time.Sleep(2 * total)
	if result.Stop.Kind != llm.StopFinished || result.Message.Text() != "done" {
		t.Fatalf("late timer changed terminal result: %+v", result)
	}
}

func TestBufferedTerminalCannotOutrunTotalTimeout(t *testing.T) {
	// The entire SSE response is in memory. No body read needs to block after
	// the sink starts, so only the completion check can catch this expiry.
	stream := "data: {\"id\":\"chat-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	httpClient := &http.Client{Transport: staticTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    r,
		}, nil
	})}
	total := 30 * time.Millisecond
	client, err := completions.New(completions.Config{
		Provider: "test", APIKey: "key", Endpoint: "https://example.test/chat",
		Client: httpClient, Timeouts: llm.HTTPTimeouts{Total: &total},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.Prepare(t.Context(), llm.Input{
		Model: "m", Stream: true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Send(t.Context(), llm.Call{
		Request: prepared,
		Sink: func(llm.Delta) {
			time.Sleep(100 * time.Millisecond)
		},
	})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorTransport ||
		failure.Reason != llm.ReasonCallTimeout ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	if result.Message.Text() != "done" || result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("partial result=%+v", result)
	}
}

func TestProtocolErrorIsNotRelabeledAsIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := completions.New(completions.Config{
		Provider: "test", APIKey: "key", Endpoint: server.URL,
		Timeouts: llm.HTTPTimeouts{StreamIdle: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.Prepare(t.Context(), llm.Input{
		Model: "m", Stream: true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Send(t.Context(), llm.Call{Request: prepared})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorProtocol || failure.Reason != "" {
		t.Fatalf("protocol error relabeled: %+v err=%v", failure, err)
	}
}
