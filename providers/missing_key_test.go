package providers_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/media"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/deepinfra"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/mistral"
	"github.com/deepnoodle-ai/dashi/providers/ollama"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/images"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/providers/openrouter"
)

var keyVariables = []string{
	"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
	"XAI_API_KEY", "GROK_API_KEY", "MISTRAL_API_KEY", "MODEL_API_KEY",
	"META_API_KEY", "DEEP_INFRA_API_KEY", "DEEPINFRA_API_KEY",
	"DEEPINFRA_TOKEN", "OLLAMA_API_KEY",
}

// clientFor builds each provider's client for its public endpoint, or with a
// custom HTTP client when custom is set.
func clientFor(name string, custom *http.Client) (llm.Client, error) {
	var client llm.Client
	var err error
	switch name {
	case "anthropic":
		client, err = messages.New(messages.Config{Client: custom})
	case "openai responses":
		client, err = responses.New(responses.Config{Client: custom})
	case "openai completions":
		client, err = completions.New(completions.Config{Client: custom})
	case "openrouter":
		client, err = openrouter.New(openrouter.Config{Client: custom})
	case "grok":
		client, err = grok.New(grok.Config{Client: custom})
	case "mistral":
		client, err = mistral.New(mistral.Config{Client: custom})
	case "deepinfra":
		client, err = deepinfra.New(deepinfra.Config{Client: custom})
	case "meta":
		client, err = meta.New(meta.Config{Client: custom})
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}

func TestMissingAPIKeyFailsAtSendBeforeDispatch(t *testing.T) {
	for _, v := range keyVariables {
		t.Setenv(v, "")
	}
	names := []string{
		"anthropic", "openai responses", "openai completions", "openrouter",
		"grok", "mistral", "deepinfra", "meta",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			client, err := clientFor(name, nil)
			if err != nil {
				t.Fatalf("New without a key: %v", err)
			}
			request, err := client.Prepare(t.Context(), modelInput())
			if err != nil {
				t.Fatalf("Prepare without a key: %v", err)
			}
			_, err = client.Send(t.Context(), llm.Call{Request: request})
			if !errors.Is(err, llm.ErrNoAPIKey) {
				t.Fatalf("Send error = %v, want ErrNoAPIKey", err)
			}
			e, ok := errors.AsType[*llm.Error](err)
			if !ok || e.Category != llm.ErrorAuthentication ||
				e.Disposition != llm.FailureNotDispatched ||
				!strings.Contains(err.Error(), "_API_KEY") {
				t.Fatalf("Send error = %#v", err)
			}

			// A custom HTTP client may add credentials itself.
			custom, err := clientFor(name, &http.Client{Transport: failingTransport{}})
			if err != nil {
				t.Fatal(err)
			}
			request, err = custom.Prepare(t.Context(), modelInput())
			if err != nil {
				t.Fatal(err)
			}
			_, err = custom.Send(t.Context(), llm.Call{Request: request})
			if errors.Is(err, llm.ErrNoAPIKey) || !errors.Is(err, errTransport) {
				t.Fatalf("custom client error = %v, want the transport's", err)
			}
		})
	}
}

func TestMissingAPIKeyExemptions(t *testing.T) {
	for _, v := range keyVariables {
		t.Setenv(v, "")
	}
	// A custom endpoint may be a proxy that supplies credentials.
	client, err := messages.New(messages.Config{
		Provider: "gateway",
		Endpoint: "http://127.0.0.1:1/v1/messages",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(t.Context(), modelInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(t.Context(), llm.Call{Request: request}); errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("custom endpoint error = %v", err)
	}
	// Ollama needs no key.
	local, err := ollama.New(ollama.Config{Endpoint: "http://127.0.0.1:1/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}
	request, err = local.Prepare(t.Context(), modelInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Send(t.Context(), llm.Call{Request: request}); errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("ollama error = %v", err)
	}
}

func TestImagesMissingKeyFailsAtSend(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	client, err := images.New(images.Config{})
	if err != nil {
		t.Fatalf("New without a key: %v", err)
	}
	request, err := client.PrepareImage(t.Context(), media.ImageInput{
		Model:     "gpt-image-1",
		Operation: media.ImageGenerate,
		Prompt:    []content.Block{content.TextBlock("a cat")},
	})
	if err != nil {
		t.Fatalf("PrepareImage without a key: %v", err)
	}
	_, err = client.SendImage(t.Context(), request)
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("SendImage error = %v, want ErrNoAPIKey", err)
	}
}

var errTransport = errors.New("fixture transport")

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errTransport
}

// recordingTransport keeps every request's headers and then fails it.
type recordingTransport struct{ headers *[]http.Header }

func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.headers = append(*r.headers, req.Header.Clone())
	return nil, errTransport
}

// A provider's key must reach only that provider. Before this test, the
// OpenAI-format and Anthropic-format clients read OPENAI_API_KEY or
// ANTHROPIC_API_KEY for any destination, so a wrapper whose own variable was
// unset sent the OpenAI key to xAI, Mistral, and the others.
func TestProviderKeysNeverReachOtherDestinations(t *testing.T) {
	const openAIKey, anthropicKey = "sk-openai-secret", "sk-ant-secret"
	for _, v := range keyVariables {
		t.Setenv(v, "")
	}
	t.Setenv("OPENAI_API_KEY", openAIKey)
	t.Setenv("ANTHROPIC_API_KEY", anthropicKey)
	for _, name := range []string{
		"openrouter", "grok", "mistral", "deepinfra", "meta", "gateway",
	} {
		t.Run(name, func(t *testing.T) {
			var headers []http.Header
			custom := &http.Client{Transport: recordingTransport{&headers}}
			var client llm.Client
			var err error
			if name == "gateway" {
				client, err = messages.New(messages.Config{
					Provider: "gateway",
					Endpoint: "https://gateway.example/v1/messages",
					Client:   custom,
				})
			} else {
				client, err = clientFor(name, custom)
			}
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), modelInput())
			if err != nil {
				t.Fatal(err)
			}
			_, _ = client.Send(t.Context(), llm.Call{Request: request})
			if len(headers) != 1 {
				t.Fatalf("requests sent = %d, want 1", len(headers))
			}
			for key, values := range headers[0] {
				for _, v := range values {
					if strings.Contains(v, openAIKey) ||
						strings.Contains(v, anthropicKey) {
						t.Fatalf("header %s carried another provider's key", key)
					}
				}
			}
		})
	}
	// The OpenAI and Anthropic destinations still read their own keys.
	var headers []http.Header
	custom := &http.Client{Transport: recordingTransport{&headers}}
	for _, name := range []string{"openai responses", "anthropic"} {
		client, err := clientFor(name, custom)
		if err != nil {
			t.Fatal(err)
		}
		request, err := client.Prepare(t.Context(), modelInput())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = client.Send(t.Context(), llm.Call{Request: request})
	}
	if len(headers) != 2 ||
		headers[0].Get("Authorization") != "Bearer "+openAIKey ||
		headers[1].Get("X-Api-Key") != anthropicKey {
		t.Fatalf("own keys not sent: %v", headers)
	}
}
