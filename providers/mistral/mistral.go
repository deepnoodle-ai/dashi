// Package mistral configures Mistral's Chat Completions endpoint. Thinking
// content parts are translated by the shared adapter. Adapted from Dive
// (65c393f), Apache-2.0.
package mistral

import (
	"context"
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

// DefaultEndpoint is Mistral's Chat Completions URL.
const DefaultEndpoint = "https://api.mistral.ai/v1/chat/completions"

// Config supplies credentials, endpoint, and application-owned model evidence.
type Config struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
	// Timeouts bounds this client's HTTP model calls.
	Timeouts         llm.HTTPTimeouts
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client uses the Chat Completions format with Mistral's destination identity.
type Client struct {
	*completions.Client
	noKey error
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
	client, err := completions.New(completions.Config{
		Provider:         "mistral",
		APIKey:           config.APIKey,
		Endpoint:         config.Endpoint,
		Client:           config.Client,
		Timeouts:         config.Timeouts,
		Models:           config.Models,
		CapabilityPolicy: config.CapabilityPolicy,
		SystemRole:       llm.RoleSystem,
	})
	if err != nil {
		return nil, err
	}
	return &Client{
		Client: client,
		noKey: providerhttp.NoAPIKey(
			client.Provider(),
			config.APIKey,
			config.Endpoint == DefaultEndpoint && config.Client == nil,
			"MISTRAL_API_KEY",
		),
	}, nil
}

// Send fails before dispatch, with an error wrapping llm.ErrNoAPIKey, when
// New found no API key for the public endpoint.
func (c *Client) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if c.noKey != nil {
		return llm.Result{}, c.noKey
	}
	return c.Client.Send(ctx, call)
}
