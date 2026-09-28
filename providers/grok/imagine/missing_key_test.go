package imagine

import (
	"errors"
	"net/http"
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
