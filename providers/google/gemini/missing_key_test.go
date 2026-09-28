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
