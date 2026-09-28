// Package meta configures Meta's Responses endpoint so signed reasoning can
// survive tool turns. Hosted web search is adapted from Dive's
// tool_web_search.go (65c393f), Apache-2.0.
package meta

import (
	"context"
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// DefaultEndpoint is Meta's Responses URL.
const DefaultEndpoint = "https://api.meta.ai/v1/responses"

// Config supplies destination credentials and application-owned model
// evidence.
type Config struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
	// Timeouts bounds this client's HTTP model calls.
	Timeouts         llm.HTTPTimeouts
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client uses the Responses format and Meta's destination identity.
type Client struct {
	*responses.Client
	noKey error
}

var _ llm.Client = (*Client)(nil)

// New uses MODEL_API_KEY, then META_API_KEY, when no explicit key is supplied.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("MODEL_API_KEY")
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("META_API_KEY")
	}
	client, err := responses.New(responses.Config{
		Provider:         "meta",
		APIKey:           config.APIKey,
		Endpoint:         config.Endpoint,
		Client:           config.Client,
		Timeouts:         config.Timeouts,
		Models:           config.Models,
		CapabilityPolicy: config.CapabilityPolicy,
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
			"MODEL_API_KEY or META_API_KEY",
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
