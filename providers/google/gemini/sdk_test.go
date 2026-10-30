package gemini

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestCaptureBodyBoundsResponseCopy(t *testing.T) {
	read := func(body string, limit int) ([]byte, *bytes.Buffer, error) {
		var out bytes.Buffer
		r := &captureBody{
			ReadCloser: io.NopCloser(strings.NewReader(body)),
			output:     &out,
			mu:         &sync.Mutex{},
			limit:      limit,
		}
		got, err := io.ReadAll(r)
		return got, &out, err
	}
	got, out, err := read("12345678", 8)
	if err != nil || string(got) != "12345678" || out.String() != "12345678" {
		t.Fatalf("body at the limit: got=%q copy=%q err=%v", got, out, err)
	}
	if _, _, err := read("123456789", 8); !errors.Is(err, errCaptureTooLarge) {
		t.Fatalf("body over the limit: err=%v", err)
	}
}

func TestSDKErrorClassifiesOversizedResponseAsProtocol(t *testing.T) {
	// The SDK returns the body's read error as is, or wrapped.
	for _, cause := range []error{errCaptureTooLarge, fmt.Errorf("read: %w", errCaptureTooLarge)} {
		var got *llm.Error
		if !errors.As(sdkError("google", cause, sdkResponse{}, false), &got) ||
			got.Category != llm.ErrorProtocol || got.Retryable {
			t.Fatalf("cause %v: got %#v", cause, got)
		}
	}
}

// sendError sends a sample request to a server that answers with status and
// body, and returns the call's error.
func sendError(t *testing.T, stream bool, handler http.HandlerFunc) *llm.Error {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(stream))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Send(t.Context(), llm.Call{Request: prepared})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	return failure
}

// HTTP errors are classified from the status and body Google sent, whatever
// shape the body has, as other providers classify theirs.
func TestSDKErrorClassifiesEveryBodyShape(t *testing.T) {
	const overflow = "The input token count exceeds the maximum number of tokens allowed."
	for _, tc := range []struct {
		name   string
		stream bool
		body   string
	}{
		{"object", false, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + overflow + `"}}`},
		{"object without code", false, `{"error":{"status":"INVALID_ARGUMENT","message":"` + overflow + `"}}`},
		{"array", true, `[{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + overflow + `"}}]`},
		{"text", false, overflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := sendError(t, tc.stream, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, tc.body)
			})
			if failure.Status != 400 ||
				failure.SendStatus != llm.SendStatusProviderRejected ||
				failure.Reason != llm.ReasonContextWindowExceeded {
				t.Fatalf("misclassified: %#v", failure)
			}
		})
	}
}

func TestSDKErrorKeepsRetryAfter(t *testing.T) {
	failure := sendError(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`)
	})
	if failure.RetryAfter != 7*time.Second || failure.Category != llm.ErrorRateLimited {
		t.Fatalf("Retry-After was lost: %#v", failure)
	}
}

// An error inside a stream that Google accepted is not a known rejection:
// output may already have streamed.
func TestSDKErrorInsideAcceptedStreamIsNotRejection(t *testing.T) {
	failure := sendError(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`+"\n\n")
		// The SDK reports a bare JSON error line as an APIError.
		io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad"}}`+"\n\n")
	})
	if failure.SendStatus != llm.SendStatusUnknown ||
		failure.Category != llm.ErrorInvalidRequest || failure.Status != 0 {
		t.Fatalf("accepted stream reported as rejected: %#v", failure)
	}
}

// An error inside a stream is classified by its own code: a 500 whose message
// reads like a context overflow is not a context-window rejection.
func TestSDKStreamErrorCodeDecidesReason(t *testing.T) {
	failure := sendError(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`+"\n\n")
		io.WriteString(w, `{"error":{"code":500,"message":"The input token count (9) exceeds the maximum number of tokens allowed (8)."}}`+"\n\n")
	})
	if failure.Reason == llm.ReasonContextWindowExceeded ||
		failure.SendStatus != llm.SendStatusUnknown ||
		failure.Category != llm.ErrorOverloaded || failure.Status != 0 {
		t.Fatalf("stream error misclassified: %#v", failure)
	}
}

// A call canceled before dispatch is known unsent, streamed or not.
func TestSendCanceledBeforeDispatchIsNotSent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var dispatched bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dispatched = true
		}))
		p := newTestClient(t, server)
		prepared, err := p.Prepare(t.Context(), sampleInput(stream))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		result, err := p.Send(ctx, llm.Call{Request: prepared})
		server.Close()
		failure, ok := errors.AsType[*llm.Error](err)
		// No response arrived, so the result has no stop kind.
		if !ok || failure.SendStatus != llm.SendStatusNotSent ||
			!errors.Is(err, context.Canceled) || dispatched ||
			result.Stop != (llm.Stop{}) {
			t.Fatalf("stream=%v: err = %#v, stop = %+v", stream, err, result.Stop)
		}
	}
}

// A connection dropped after the request reached the server may have run the
// call, so the result keeps a stop kind and the send status stays unknown.
func TestSendDroppedAfterDispatchKeepsStopKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(false))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.SendStatus == llm.SendStatusNotSent ||
		failure.Status != 0 || result.Stop.Kind != llm.StopIncomplete {
		t.Fatalf("result = %+v, err = %#v", result, err)
	}
}

// The SDK stream path gives text fragments of one part one index, as the
// assembled message does.
func TestSDKStreamDeltaIndexesMatchMessageParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"responseId":"r1","candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"lo"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{}}}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`,
		} {
			io.WriteString(w, "data: "+chunk+"\n\n")
		}
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(t.Context(), sampleInput(true))
	if err != nil {
		t.Fatal(err)
	}
	var indexes []int
	result, err := p.Send(t.Context(), llm.Call{
		Request: prepared,
		Sink: func(d llm.Delta) {
			if d.Kind != llm.DeltaUsage {
				indexes = append(indexes, d.Index)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Message.Content) != 3 || fmt.Sprint(indexes) != "[0 0 1 2]" {
		t.Fatalf("indexes = %v, parts = %d", indexes, len(result.Message.Content))
	}
}

func TestQuotaProjectHeader(t *testing.T) {
	var got string
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("X-Goog-User-Project")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	request, _ := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if _, err := withQuotaProject(next, "billing").RoundTrip(request); err != nil || got != "billing" {
		t.Fatalf("header = %q, err = %v", got, err)
	}
	if withQuotaProject(next, "") == nil {
		t.Fatal("empty quota project dropped the transport")
	}
}

// Timeouts bound SDK calls as they bound other providers' HTTP calls, with
// the same error reasons, and a timed-out stream keeps its partial result.
func TestSendReportsTimeoutReasons(t *testing.T) {
	const chunk = `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n"
	total := 150 * time.Millisecond
	for _, tc := range []struct {
		name     string
		stream   bool
		timeouts llm.HTTPTimeouts
		handler  http.HandlerFunc
		want     llm.ErrorReason
	}{
		{
			name:     "response start",
			timeouts: llm.HTTPTimeouts{ResponseStart: 50 * time.Millisecond},
			handler: func(_ http.ResponseWriter, r *http.Request) {
				// The server may not see the client give up before headers.
				select {
				case <-r.Context().Done():
				case <-time.After(500 * time.Millisecond):
				}
			},
			want: llm.ReasonResponseStartTimeout,
		},
		{
			name:     "stream idle",
			stream:   true,
			timeouts: llm.HTTPTimeouts{StreamIdle: 80 * time.Millisecond},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, chunk)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
			want: llm.ReasonStreamIdleTimeout,
		},
		{
			// Total bounds the whole call, so it ends an active stream.
			name:     "total",
			stream:   true,
			timeouts: llm.HTTPTimeouts{StreamIdle: 80 * time.Millisecond, Total: &total},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for {
					io.WriteString(w, chunk)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			},
			want: llm.ReasonCallTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			p, err := New(Config{
				Provider:         "google",
				APIKey:           "test",
				Endpoint:         server.URL + "/v1beta/models/{model}:generateContent",
				CapabilityPolicy: llm.CapabilityAdjust,
				Timeouts:         tc.timeouts,
			})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := p.Prepare(t.Context(), sampleInput(tc.stream))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result, err := p.Send(ctx, llm.Call{Request: prepared})
			failure, ok := errors.AsType[*llm.Error](err)
			if !ok || failure.Category != llm.ErrorTransport ||
				failure.Reason != tc.want ||
				!errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want reason %s", err, tc.want)
			}
			if tc.stream && result.Message.Text() == "" {
				t.Fatalf("stream lost its partial result: %+v", result)
			}
		})
	}
}

func TestNewRejectsNegativeTimeoutsAndOwnsTotal(t *testing.T) {
	negative := -time.Second
	for _, timeouts := range []llm.HTTPTimeouts{
		{ResponseStart: -1}, {StreamIdle: -1}, {Total: &negative},
	} {
		if _, err := New(Config{APIKey: "test", Timeouts: timeouts}); err == nil {
			t.Fatalf("accepted negative timeouts: %+v", timeouts)
		}
	}
	total := time.Second
	p, err := New(Config{APIKey: "test", Timeouts: llm.HTTPTimeouts{Total: &total}})
	if err != nil {
		t.Fatal(err)
	}
	total = time.Minute
	if *p.config.Timeouts.Total != time.Second {
		t.Fatalf("total stayed caller-owned: %v", *p.config.Timeouts.Total)
	}
}
