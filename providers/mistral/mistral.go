// Package mistral configures Mistral's Chat Completions endpoint. Thinking
// content parts are translated by the shared adapter. Adapted from Dive
// (65c393f), Apache-2.0.
package mistral

import (
	"net/http"
	"os"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
)

// DefaultEndpoint is Mistral's Chat Completions URL.
const DefaultEndpoint = "https://api.mistral.ai/v1/chat/completions"

// Config supplies credentials, endpoint, and application-owned model evidence.
type Config struct {
	APIKey              string
	Endpoint            string
	Client              *http.Client
	Models              map[string]llm.ModelInfo
	UnknownCapabilities llm.UnknownPolicy
}

// Client uses the Chat Completions format with Mistral's destination identity.
type Client struct {
	*completions.Client
}

var _ llm.Client = (*Client)(nil)

// New passes native Mistral model IDs through unchanged.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("MISTRAL_API_KEY")
	}
	client, err := completions.New(completions.Config{Provider: "mistral", APIKey: config.APIKey,
		Endpoint: config.Endpoint, Client: config.Client, Models: config.Models,
		UnknownCapabilities: config.UnknownCapabilities, SystemRole: llm.RoleSystem})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
