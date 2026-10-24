package completions

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// sendStream serves the events as one SSE body and sends a streamed call.
func sendStream(t *testing.T, events ...string) (llm.Result, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(true))
	if err != nil {
		t.Fatal(err)
	}
	return p.Send(t.Context(), llm.Call{Request: prepared})
}

// TestStreamCutOffKeepsToolCallsPartial covers a stream that ends before its
// finish reason. A tool call whose arguments happen to be valid JSON was
// never confirmed by the provider, so it must not become an executable call.
func TestStreamCutOffKeepsToolCallsPartial(t *testing.T) {
	result, err := sendStream(t,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]}}]}`,
	)
	if err == nil {
		t.Fatal("cut-off stream succeeded")
	}
	if len(result.Message.Content) != 1 {
		t.Fatalf("content = %+v, want one partial call", result.Message.Content)
	}
	c := result.Message.Content[0]
	if c.Kind != llm.ContentOpaque || c.Opaque == nil || !c.Opaque.Partial {
		t.Fatalf("content = %+v, want partial opaque tool call", c)
	}
}

// TestSendFailureBeforeResponseHasNoStop covers a call cancelled before
// dispatch. A stop kind marks a call that may have run, so a call that was
// definitely not sent must not carry one.
func TestSendFailureBeforeResponseHasNoStop(t *testing.T) {
	p := testClient(t, "http://127.0.0.1:1")
	prepared, err := p.Prepare(t.Context(), testInput(true))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := p.Send(ctx, llm.Call{Request: prepared})
	if err == nil || result.Stop.Kind != "" {
		t.Fatalf("stop = %q, err = %v; want no stop and an error", result.Stop.Kind, err)
	}
}

// TestSendHTTPErrorKeepsStop covers an HTTP error status. Any status means the
// provider received the request and may have run it, so the call keeps a stop
// kind and is never taken for one that was not sent.
func TestSendHTTPErrorKeepsStop(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		p := testClient(t, server.URL)
		prepared, err := p.Prepare(t.Context(), testInput(true))
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.Send(t.Context(), llm.Call{Request: prepared})
		server.Close()
		if err == nil || result.Stop.Kind != llm.StopIncomplete {
			t.Fatalf("status %d: stop = %q, err = %v; want incomplete and an error",
				status, result.Stop.Kind, err)
		}
	}
}

// TestSendDroppedConnectionKeepsStop covers a connection that closes after the
// request was written but before any response. The provider may have run the
// call, so it keeps a stop kind.
func TestSendDroppedConnectionKeepsStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(true))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err == nil || result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("stop = %q, err = %v; want incomplete and an error", result.Stop.Kind, err)
	}
}
