// Package grok configures xAI's Responses endpoint. Adapted from Dive
// (65c393f), Apache-2.0.
package grok

import (
	"net/http"
	"os"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

// DefaultEndpoint is xAI's Responses URL.
const DefaultEndpoint = "https://api.x.ai/v1/responses"

// Config supplies destination credentials and application-owned model evidence.
type Config struct {
	APIKey              string
	Endpoint            string
	Client              *http.Client
	Models              map[string]llm.ModelInfo
	UnknownCapabilities llm.UnknownPolicy
}

// Client uses the Responses wire format with Grok's destination identity.
type Client struct {
	*responses.Client
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
	client, err := responses.New(responses.Config{Provider: "grok", APIKey: config.APIKey,
		Endpoint: config.Endpoint, Client: config.Client, Models: config.Models,
		UnknownCapabilities: config.UnknownCapabilities})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
