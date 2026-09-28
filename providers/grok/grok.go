// Package grok configures xAI's Responses endpoint. The hosted options are
// adapted from Dive's web search, X search, collections search, and code
// execution tools (65c393f), Apache-2.0.
//
// # Hosted tools
//
// Hosted tool constructors return a definition-only tool.Tool: xAI runs it
// inside the model call, so it has no Executor and no hook sees its calls.
// Add it to a Runner with tool.List, or pass its Definition in llm.Input.Tools. A
// constructor returns an error only when its options can be invalid. These
// tools are bound to the destination named "grok" that New configures.
package grok

import (
	"context"
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// DefaultEndpoint is xAI's Responses URL.
const DefaultEndpoint = "https://api.x.ai/v1/responses"

// Config supplies destination credentials and application-owned model
// evidence.
type Config struct {
	APIKey           string
	Endpoint         string
	Client           *http.Client
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client uses the Responses wire format with Grok's destination identity.
type Client struct {
	*responses.Client
	noKey error
}

var _ llm.Client = (*Client)(nil)

// New selects xAI's endpoint without changing native model IDs.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("XAI_API_KEY")
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("GROK_API_KEY")
	}
	client, err := responses.New(responses.Config{
		Provider:         "grok",
		APIKey:           config.APIKey,
		Endpoint:         config.Endpoint,
		Client:           config.Client,
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
			"XAI_API_KEY or GROK_API_KEY",
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
