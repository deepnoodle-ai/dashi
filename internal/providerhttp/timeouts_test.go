package providerhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func timeoutRequest(
	t *testing.T,
	endpoint string,
	httpClient *http.Client,
	limits llm.HTTPTimeouts,
) (Client, llm.Request) {
	t.Helper()
	client, err := NewClient(
		"test",
		"test.chat",
		endpoint,
		"",
		httpClient,
		nil,
		nil,
		llm.CapabilityAdjust,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model: "m", Stream: true,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}
	prepared, err := client.Prepare(
		input,
		input,
		map[string]any{"model": "m"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return client, prepared
}

func requireTimeoutReason(t *testing.T, err error, want llm.ErrorReason) {
	t.Helper()
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorTransport || failure.Reason != want ||
		failure.SendStatus != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failure=%+v err=%v, want reason %s", failure, err, want)
	}
}

func TestResponseStartTimeoutKeepsDispatchResultUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer server.Close()
	client, prepared := timeoutRequest(t, server.URL, nil, llm.HTTPTimeouts{
		ResponseStart: 50 * time.Millisecond,
	})
	_, err := client.Post(t.Context(), prepared)
	requireTimeoutReason(t, err, llm.ReasonResponseStartTimeout)
}

func TestStreamIdleMeasuresBlockedReadsAndHeartbeatBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer server.Close()
	client, prepared := timeoutRequest(t, server.URL, nil, llm.HTTPTimeouts{
		StreamIdle: 80 * time.Millisecond,
	})
	resp, err := client.Post(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != ": heartbeat\n\n: heartbeat\n\n: heartbeat\n\ndone" {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

func TestStreamIdleIgnoresConsumerTime(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "b")
	}))
	defer server.Close()
	client, prepared := timeoutRequest(t, server.URL, nil, llm.HTTPTimeouts{
		StreamIdle: 80 * time.Millisecond,
	})
	resp, err := client.Post(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "a" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	time.Sleep(110 * time.Millisecond)
	close(release)
	second, err := io.ReadAll(resp.Body)
	if err != nil || string(second) != "b" {
		t.Fatalf("second=%q err=%v", second, err)
	}
}

func TestTotalTimeoutEndsActiveStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer server.Close()
	total := 110 * time.Millisecond
	client, prepared := timeoutRequest(t, server.URL, nil, llm.HTTPTimeouts{
		StreamIdle: 80 * time.Millisecond,
		Total:      &total,
	})
	resp, err := client.Post(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if len(got) < 2 {
		t.Fatalf("active stream yielded only %q", got)
	}
	requireTimeoutReason(t, ReadFailure("test", err), llm.ReasonCallTimeout)
}

func TestCustomClientTimeoutDoesNotBecomeDashiReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer server.Close()
	custom := &http.Client{Timeout: 40 * time.Millisecond}
	zero := time.Duration(0)
	client, prepared := timeoutRequest(t, server.URL, custom, llm.HTTPTimeouts{
		ResponseStart: time.Second,
		Total:         &zero,
	})
	_, err := client.Post(t.Context(), prepared)
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Reason != "" || custom.Timeout != 40*time.Millisecond {
		t.Fatalf("failure=%+v err=%v custom=%+v", failure, err, custom)
	}
}

func TestParentCancellationIsNotRelabeledAsProviderTimeout(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer server.Close()
	client, prepared := timeoutRequest(t, server.URL, nil, llm.HTTPTimeouts{
		ResponseStart: time.Second,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Post(ctx, prepared)
		done <- err
	}()
	<-entered
	cancel()
	failure, ok := errors.AsType[*llm.Error](<-done)
	if !ok || failure.Reason != "" || !errors.Is(failure, context.Canceled) {
		t.Fatalf("parent cancel became timeout: %+v", failure)
	}
}

func TestNegativeTimeoutsRejectedAndTotalIsSnapshotted(t *testing.T) {
	for _, limits := range []llm.HTTPTimeouts{
		{ResponseStart: -1}, {StreamIdle: -1},
		{Total: func() *time.Duration { v := time.Duration(-1); return &v }()},
	} {
		if _, err := NewClient(
			"test",
			"test.chat",
			"http://localhost",
			"",
			nil,
			nil,
			nil,
			llm.CapabilityAdjust,
			limits,
		); err == nil {
			t.Fatalf("accepted negative timeout: %+v", limits)
		}
	}
	total := time.Second
	client, _ := timeoutRequest(t, "http://localhost", nil, llm.HTTPTimeouts{Total: &total})
	total = 2 * time.Second
	if *client.timeouts.Total != time.Second {
		t.Fatalf("timeout pointer remained caller-owned: %v", *client.timeouts.Total)
	}
}

func TestTimeoutTransportTotalMatchesPost(t *testing.T) {
	zero, one := time.Duration(0), time.Second
	for _, tc := range []struct {
		name     string
		total    *time.Duration
		supplied bool
		want     time.Duration
	}{
		{"zero value, built client", nil, false, 5 * time.Minute},
		{"zero value, supplied client", nil, true, 0},
		{"explicit zero", &zero, false, 0},
		{"explicit total, supplied client", &one, true, time.Second},
	} {
		transport := TimeoutTransport(
			http.DefaultTransport,
			llm.HTTPTimeouts{Total: tc.total},
			tc.supplied,
			true,
		).(timeoutTransport)
		if transport.total != tc.want {
			t.Fatalf("%s: total=%v, want %v", tc.name, transport.total, tc.want)
		}
	}
}

func TestTimeoutTransportReportsResponseStartTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: TimeoutTransport(
		http.DefaultTransport,
		llm.HTTPTimeouts{ResponseStart: 50 * time.Millisecond},
		false,
		false,
	)}
	_, err := client.Get(server.URL)
	if TimeoutReason(err) != llm.ReasonResponseStartTimeout {
		t.Fatalf("err=%v", err)
	}
	requireTimeoutReason(t, ReadFailure("test", err), llm.ReasonResponseStartTimeout)
}

func TestExpiredReportsOnlyDashiTimeoutsAfterClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "done")
	}))
	defer server.Close()
	get := func(ctx context.Context, total time.Duration) io.ReadCloser {
		t.Helper()
		client := &http.Client{Transport: TimeoutTransport(
			http.DefaultTransport,
			llm.HTTPTimeouts{Total: &total},
			false,
			false,
		)}
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		return resp.Body
	}

	// The total timer fired after the body was read but before it closed.
	late := get(t.Context(), 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	late.Close()
	requireTimeoutReason(t, Expired("test", late), llm.ReasonCallTimeout)

	// Closing in time, or a parent's cancellation, is not a timeout.
	inTime := get(t.Context(), time.Minute)
	inTime.Close()
	if err := Expired("test", inTime); err != nil {
		t.Fatalf("in-time close: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	canceled := get(ctx, time.Minute)
	cancel()
	canceled.Close()
	if err := Expired("test", canceled); err != nil {
		t.Fatalf("parent cancel: %v", err)
	}
	if err := Expired("test", io.NopCloser(nil)); err != nil {
		t.Fatalf("foreign body: %v", err)
	}
}
