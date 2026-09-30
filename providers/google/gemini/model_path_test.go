package gemini

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestModelIDIsOnePathSegmentThroughSDK(t *testing.T) {
	for _, model := range []string{"gemini-fixture", "acme/code-v1", "models/custom-v1"} {
		for _, route := range []string{"public", "custom", "vertex"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", route, model, stream), func(t *testing.T) {
					dispatches := 0
					var sentBody []byte
					var sentURL *url.URL
					transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
						dispatches++
						sentURL = r.URL
						var err error
						sentBody, err = io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						body := `{"modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
						kind := "application/json"
						if stream {
							body = "data: " + body + "\n\n"
							kind = "text/event-stream"
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					})
					config := Config{Provider: "google", APIKey: "fixture", Client: &http.Client{Transport: transport}}
					if route == "custom" {
						config.Provider = "proxy"
						config.Endpoint = "https://proxy.example/v1/models/{model}:generateContent"
					}
					if route == "vertex" {
						config.Provider = "vertex"
						config.APIKey = ""
						config.Vertex = true
						config.Project = "project"
						config.Location = "global"
						config.CredentialScope = "fixture-principal"
					}
					client, err := New(config)
					if err != nil {
						t.Fatal(err)
					}
					request, err := client.Prepare(t.Context(), llm.Input{Model: model, Stream: stream, Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
					if err != nil {
						t.Fatal(err)
					}
					if dispatches != 0 {
						t.Fatalf("Prepare dispatched %d requests", dispatches)
					}
					if request.Input().Model != model || request.Snapshot().Input.Model != model || request.Origin().Model != model {
						t.Fatal("recorded model identity changed")
					}
					result, err := client.Send(t.Context(), llm.Call{Request: request})
					if err != nil {
						t.Fatal(err)
					}
					suffix := "/models/" + url.PathEscape(model) + ":generateContent"
					if stream {
						suffix = "/models/" + url.PathEscape(model) + ":streamGenerateContent"
					}
					if dispatches != 1 || !strings.HasSuffix(sentURL.EscapedPath(), suffix) || strings.Contains(sentURL.EscapedPath(), "%252F") {
						t.Fatalf("dispatches=%d url=%s expected suffix=%s", dispatches, sentURL, suffix)
					}
					if result.Requested.Model != model || result.Message.Text() != "ok" || !bytes.Equal(sentBody, request.Body()) {
						t.Fatalf("identity, output or prepared body changed: %+v", result)
					}
				})
			}
		}
	}
}
