package imagine

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
)

func TestMissingAPIKeyFailsAtSend(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_API_KEY", "")
	client, err := New(Config{})
	if err != nil {
		t.Fatalf("New without a key: %v", err)
	}
	_, err = client.do(t.Context(), http.MethodPost, "/images/generations", nil, "")
	e, ok := errors.AsType[*media.Error](err)
	if !errors.Is(err, llm.ErrNoAPIKey) || !ok ||
		e.Category != media.ErrorAuthentication {
		t.Fatalf("send error = %v, want an authentication ErrNoAPIKey", err)
	}
	custom, err := New(Config{Client: &http.Client{}})
	if err != nil || custom.noKey != nil {
		t.Fatalf("custom HTTP client: noKey=%v err=%v", custom.noKey, err)
	}
}

type recordingTransport struct{ headers *[]http.Header }

func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.headers = append(*r.headers, req.Header.Clone())
	return nil, errors.New("fixture transport")
}

func TestXAIKeyNeverReachesAnotherProvider(t *testing.T) {
	t.Setenv("XAI_API_KEY", "xai-secret")
	t.Setenv("GROK_API_KEY", "grok-secret")
	var headers []http.Header
	gateway, err := New(Config{
		Provider: "gateway",
		Endpoint: "https://gateway.example/v1",
		Client:   &http.Client{Transport: recordingTransport{&headers}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = gateway.do(t.Context(), http.MethodPost, "/images/generations", nil, "")
	if len(headers) != 1 {
		t.Fatalf("requests sent = %d, want 1", len(headers))
	}
	if auth := headers[0].Get("Authorization"); strings.Contains(auth, "secret") {
		t.Fatalf("gateway received an xAI key: %q", auth)
	}
	// The grok destination still reads its own key, and an explicit key is
	// kept for any destination.
	grok, err := New(Config{})
	if err != nil || grok.apiKey != "xai-secret" {
		t.Fatalf("grok key = %q, err = %v", grok.apiKey, err)
	}
	explicit, err := New(Config{
		Provider: "gateway",
		Endpoint: "https://gateway.example/v1",
		APIKey:   "gateway-key",
	})
	if err != nil || explicit.apiKey != "gateway-key" {
		t.Fatalf("explicit key = %q, err = %v", explicit.apiKey, err)
	}
}
