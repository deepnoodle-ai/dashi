// Package openrouter configures OpenRouter's Chat Completions endpoint.
// Adapted
// from Dive (65c393f), Apache-2.0.
package openrouter

import (
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

// DefaultEndpoint is OpenRouter's Chat Completions URL.
const DefaultEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// Config supplies credentials, endpoint, application-owned model evidence, and
// the attribution headers OpenRouter displays to model providers and users.
type Config struct {
	APIKey           string
	Endpoint         string
	Client           *http.Client
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
	SiteURL          string
	SiteName         string
}

// Client retains OpenRouter's destination identity and reported usage cost.
type Client struct {
	*completions.Client
}

var _ llm.Client = (*Client)(nil)

// New passes native OpenRouter model IDs through unchanged. Site metadata
// defaults to Deep Noodle, as in Dive.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		config.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if config.SiteURL == "" {
		config.SiteURL = "https://deepnoodle.ai"
	}
	if config.SiteName == "" {
		config.SiteName = "Deep Noodle"
	}
	client, err := completions.New(completions.Config{
		Provider:          "openrouter",
		APIKey:            config.APIKey,
		Endpoint:          config.Endpoint,
		Client:            config.Client,
		Models:            config.Models,
		CapabilityPolicy:  config.CapabilityPolicy,
		SystemRole:        llm.RoleSystem,
		ReportedCostField: "cost",
		CostCurrency:      "USD",
		Headers: http.Header{
			"Http-Referer": {config.SiteURL},
			"X-Title":      {config.SiteName},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
