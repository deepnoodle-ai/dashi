// Package meta configures Meta's Responses endpoint so signed reasoning can
// survive tool turns. Hosted web search is adapted from Dive's
// tool_web_search.go (65c393f), Apache-2.0.
package meta

import (
	"net/http"
	"os"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

// DefaultEndpoint is Meta's Responses URL.
const DefaultEndpoint = "https://api.meta.ai/v1/responses"

// Config supplies destination credentials and application-owned model
// evidence.
type Config struct {
	APIKey           string
	Endpoint         string
	Client           *http.Client
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client uses the Responses format and Meta's destination identity.
type Client struct {
	*responses.Client
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
		Models:           config.Models,
		CapabilityPolicy: config.CapabilityPolicy,
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
