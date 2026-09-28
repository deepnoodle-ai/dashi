package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestErrorReason(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		media  bool
		want   llm.ErrorReason
	}{
		{
			name:   "token count",
			status: 400,
			body:   `{"error":{"code":400,"message":"The input token count (1250000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
			want:   llm.ReasonContextWindowExceeded,
		},
		{
			name:   "unprocessable image",
			status: 400,
			media:  true,
			body:   `{"error":{"code":400,"message":"Unable to process input image. Please retry or report in https://developers.generativeai.google/guide/troubleshooting","status":"INVALID_ARGUMENT"}}`,
			want:   llm.ReasonMediaRejected,
		},
		{
			name:   "unsupported mime type",
			status: 400,
			media:  true,
			body:   `{"error":{"code":400,"message":"Unsupported MIME type: image/tiff","status":"INVALID_ARGUMENT"}}`,
			want:   llm.ReasonMediaRejected,
		},
		{
			name:   "payload size with media",
			status: 400,
			media:  true,
			body:   `{"error":{"code":400,"message":"Request payload size exceeds the limit: 20971520 bytes.","status":"INVALID_ARGUMENT"}}`,
			want:   llm.ReasonMediaRejected,
		},
		{
			name:   "payload size without media",
			status: 400,
			body:   `{"error":{"code":400,"message":"Request payload size exceeds the limit: 20971520 bytes.","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name:   "generic argument",
			status: 400,
			media:  true,
			body:   `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name:   "quota",
			status: 429,
			body:   `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
		},
		{
			name:   "precondition",
			status: 400,
			body:   `{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := providerhttp.ParseRejection(tt.status, []byte(tt.body))
			r.HasMedia = tt.media
			if got := errorReason(r); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSDKErrorCarriesReason(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		media      bool
		want       llm.ErrorReason
	}{
		{
			name:   "generic 413 with media",
			status: http.StatusRequestEntityTooLarge,
			media:  true,
			want:   llm.ReasonMediaRejected,
			body:   `{"error":{"code":413,"message":"Request Entity Too Large","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name:   "generic 413 without media",
			status: http.StatusRequestEntityTooLarge,
			body:   `{"error":{"code":413,"message":"Request Entity Too Large","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name: "context",
			want: llm.ReasonContextWindowExceeded,
			body: `{"error":{"code":400,"message":"The input token count (1250000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name:  "media",
			media: true,
			want:  llm.ReasonMediaRejected,
			body:  `{"error":{"code":400,"message":"Unable to process input image.","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name: "media phrase without media",
			body: `{"error":{"code":400,"message":"Unable to process input image.","status":"INVALID_ARGUMENT"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := tt.status
				if status == 0 {
					status = http.StatusBadRequest
				}
				w.WriteHeader(status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			p := newTestClient(t, server)
			input := sampleInput(false)
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
			var failure *llm.Error
			if !errors.As(
				err,
				&failure,
			) ||
				failure.Category != llm.ErrorInvalidRequest ||
				failure.Reason != tt.want {
				t.Fatalf("err = %v, want invalid_request with reason %q", err, tt.want)
			}
			if tt.status != 0 && failure.Status != tt.status {
				t.Fatalf("err = %v, want invalid_request with reason %q", err, tt.want)
			}
		})
	}
}

func TestStreamErrorCarriesReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(
			w,
			"data: "+`{"error":{"code":400,"message":"The input token count (1250000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`+"\n\n",
		)
	}))
	defer server.Close()
	p := newTestClient(t, server)
	prepared, err := p.Prepare(context.Background(), sampleInput(true))
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
	if !llm.IsContextWindowExceeded(err) {
		t.Fatalf("err = %v, want context window exceeded", err)
	}
}
