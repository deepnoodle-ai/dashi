package gemini

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestSDKDispatchUsesOnlyResolvedDestinationCredential(t *testing.T) {
	for _, environment := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		for _, key := range []string{"", "selected-destination-key"} {
			for _, stream := range []bool{false, true} {
				t.Run(environment+"/"+key+map[bool]string{false: "/plain", true: "/stream"}[stream], func(t *testing.T) {
					t.Setenv("GEMINI_API_KEY", "")
					t.Setenv("GOOGLE_API_KEY", "")
					t.Setenv(environment, "native-google-fixture-key")
					dispatches := 0
					client, err := New(Config{Provider: "custom", APIKey: key, Endpoint: "https://custom.example/v1beta/models/{model}:generateContent", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						dispatches++
						if got := r.Header.Get("X-Goog-Api-Key"); got != key {
							t.Errorf("sent key=%q want selected key=%q", got, key)
						}
						for _, values := range r.Header {
							for _, value := range values {
								if strings.Contains(value, "native-google-fixture-key") || strings.Contains(value, "dashi-sdk-keyless") {
									t.Error("unselected credential or placeholder dispatched")
								}
							}
						}
						body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
						kind := "application/json"
						if stream {
							kind = "text/event-stream"
							body = "data: " + body + "\n\n"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					})}})
					if err != nil {
						t.Fatal(err)
					}
					request, err := client.Prepare(t.Context(), llm.Input{Model: "exact/model", Stream: stream, Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
					if err != nil || dispatches != 0 {
						t.Fatalf("preparation error=%v calls=%d", err, dispatches)
					}
					result, err := client.Send(t.Context(), llm.Call{Request: request})
					if err != nil || dispatches != 1 || result.Message.Text() != "ok" {
						t.Fatalf("send result=%+v error=%v calls=%d", result, err, dispatches)
					}
				})
			}
		}
	}
}

func TestKeylessCustomSDKPreparationNeedsNoAmbientCredentials(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	client, err := New(Config{Provider: "keyless", Endpoint: "https://custom.example/v1beta/models/{model}:generateContent", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("Prepare dispatched"); return nil, nil })}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Prepare(t.Context(), llm.Input{Model: "exact", Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}}); err != nil {
		t.Fatal(err)
	}
}
