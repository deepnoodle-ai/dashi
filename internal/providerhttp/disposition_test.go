package providerhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPostReportsOnlyProvenNoEffect(t *testing.T) {
	for _, tc := range []struct {
		status      int
		disposition llm.FailureDisposition
	}{
		{
			400,
			llm.FailureRejected,
		},
		{
			429,
			llm.FailureRejected,
		},
		{
			408,
			llm.FailureUnknown,
		},
		{
			409,
			llm.FailureUnknown,
		},
		{
			503,
			llm.FailureUnknown,
		},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer server.Close()
			client, err := NewClient(
				"test",
				"test.chat",
				server.URL,
				"",
				nil,
				nil,
				nil,
				llm.CapabilityAdjust,
			)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(
				llm.Input{
					Model: "m",
					Messages: []llm.Message{
						llm.UserMessage(llm.Text("hello")),
					},
				},
				llm.Input{
					Model: "m",
					Messages: []llm.Message{
						llm.UserMessage(llm.Text("hello")),
					},
				},
				map[string]any{
					"model": "m",
				},
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Post(
				context.Background(),
				request,
			)
			var failure *llm.Error
			if !errors.As(
				err,
				&failure,
			) || failure.Disposition != tc.disposition {
				t.Fatalf(
					"failure=%+v err=%v",
					failure,
					err,
				)
			}
		})
	}
}

func TestCanceledPostIsKnownNotDispatched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request dispatched") }))
	defer server.Close()
	client, err := NewClient(
		"test",
		"test.chat",
		server.URL,
		"",
		nil,
		nil,
		nil,
		llm.CapabilityAdjust,
	)
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model: "m",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("hello")),
		},
	}
	request, err := client.Prepare(
		input,
		input,
		map[string]any{
			"model": "m",
		},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Post(ctx, request)
	var failure *llm.Error
	if !errors.Is(
		err,
		context.Canceled,
	) || !errors.As(
		err,
		&failure,
	) || failure.Disposition != llm.FailureNotDispatched {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
}
