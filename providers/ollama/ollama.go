// Package ollama configures Ollama's Anthropic-compatible Messages endpoint.
// Adapted from Dive (65c393f), Apache-2.0.
package ollama

import (
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

// DefaultEndpoint is Ollama's local Messages-compatible URL.
const DefaultEndpoint = "http://localhost:11434/v1/messages"

// Config supplies optional credentials, endpoint, and model evidence.
type Config struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
	// Timeouts bounds this client's HTTP model calls.
	Timeouts         llm.HTTPTimeouts
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client uses the Messages wire format with Ollama's destination identity.
type Client struct {
	*messages.Client
}

var _ llm.Client = (*Client)(nil)

// New uses Ollama's local endpoint and the placeholder key its Messages
// compatibility API expects when no credential is configured.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("OLLAMA_API_KEY")
	}
	if config.APIKey == "" {
		config.APIKey = "ollama"
	}
	client, err := messages.New(messages.Config{
		Provider:         "ollama",
		APIKey:           config.APIKey,
		Endpoint:         config.Endpoint,
		Client:           config.Client,
		Timeouts:         config.Timeouts,
		Models:           config.Models,
		CapabilityPolicy: config.CapabilityPolicy,
		// Ollama does not implement Anthropic's task budget beta.
		Unsupported: []llm.Feature{llm.FeatureTaskBudget},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
