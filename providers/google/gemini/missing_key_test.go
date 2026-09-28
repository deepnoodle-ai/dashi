package gemini

import (
	"errors"
	"net/http"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestMissingAPIKeyFailsAtSend(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	client, err := New(Config{})
	if err != nil {
		t.Fatalf("New without a key: %v", err)
	}
	_, err = client.Send(t.Context(), llm.Call{})
	e, ok := errors.AsType[*llm.Error](err)
	if !errors.Is(err, llm.ErrNoAPIKey) || !ok ||
		e.Disposition != llm.FailureNotDispatched {
		t.Fatalf("Send error = %v, want a not-dispatched ErrNoAPIKey", err)
	}
	custom, err := New(Config{Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if custom.noKey != nil {
		t.Fatal("a custom HTTP client still requires a key")
	}
}

func TestMediaMissingAPIKeyFailsAtSend(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	mediaClient, err := NewMedia(MediaConfig{})
	if err != nil {
		t.Fatalf("NewMedia without a key: %v", err)
	}
	_, err = mediaClient.do(t.Context(), http.MethodPost, DefaultImageEndpoint, nil, "", 1, false)
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("media send error = %v, want ErrNoAPIKey", err)
	}
	interactions, err := NewInteractionsMedia(InteractionsMediaConfig{})
	if err != nil {
		t.Fatalf("NewInteractionsMedia without a key: %v", err)
	}
	if _, err := interactions.do(t.Context(), nil); !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("interactions send error = %v, want ErrNoAPIKey", err)
	}
	custom, err := NewMedia(MediaConfig{Client: &http.Client{}})
	if err != nil || custom.noKey != nil {
		t.Fatalf("custom HTTP client: noKey=%v err=%v", custom.noKey, err)
	}
}

func TestGeminiKeyStaysWithGoogle(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-secret")
	t.Setenv("GOOGLE_API_KEY", "")
	proxy, err := New(Config{
		Provider: "proxy",
		Endpoint: "https://proxy.example/v1beta/models/{model}:generateContent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.config.APIKey != "" {
		t.Fatal("a non-Google destination read GEMINI_API_KEY")
	}
	google, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if google.config.APIKey != "gemini-secret" {
		t.Fatal("the Google destination did not read GEMINI_API_KEY")
	}
}
