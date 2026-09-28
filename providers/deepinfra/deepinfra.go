// Package deepinfra configures Dive's DeepInfra Chat Completions destination
// for Dashi's prepared request contract. Adapted from Dive (65c393f),
// Apache-2.0.
package deepinfra

import (
	"context"
	"net/http"
	"os"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/providerhttp"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

// DefaultEndpoint is DeepInfra's Chat Completions URL.
const DefaultEndpoint = "https://api.deepinfra.com/v1/openai/chat/completions"

// Config supplies destination credentials and application-owned model
// evidence.
type Config struct {
	APIKey           string
	Endpoint         string
	Client           *http.Client
	Models           map[string]llm.ModelInfo
	CapabilityPolicy llm.CapabilityPolicy
}

// Client reports DeepInfra's usage.estimated_cost as a provider estimate.
type Client struct {
	*completions.Client
	noKey error
}

var _ llm.Client = (*Client)(nil)

// New uses the native model ID unchanged. DeepInfra's API does not accept
// binary documents in this Chat Completions dialect.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.APIKey == "" {
		for _, name := range []string{
			"DEEP_INFRA_API_KEY",
			"DEEPINFRA_API_KEY",
			"DEEPINFRA_TOKEN",
		} {
			if value := os.Getenv(name); value != "" {
				config.APIKey = value
				break
			}
		}
	}
	client, err := completions.New(completions.Config{
		Provider:          "deepinfra",
		APIKey:            config.APIKey,
		Endpoint:          config.Endpoint,
		Client:            config.Client,
		Models:            config.Models,
		CapabilityPolicy:  config.CapabilityPolicy,
		SystemRole:        llm.RoleSystem,
		ReportedCostField: "estimated_cost",
		CostCurrency:      "USD",
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
			"DEEP_INFRA_API_KEY, DEEPINFRA_API_KEY, or DEEPINFRA_TOKEN",
		),
	}, nil
}

// Prepare rejects media documents before dispatch because DeepInfra's Chat
// Completions endpoint requires rendered page images for visual document
// input.
func (p *Client) Prepare(
	ctx context.Context,
	input llm.Input,
) (llm.Request, error) {
	for _, message := range input.Messages {
		for _, part := range message.Content {
			if part.Block != nil && part.Block.Kind == content.KindDocument {
				return llm.Request{}, &llm.Error{
					Provider: p.Provider(),
					Category: llm.ErrorUnsupported,
					Cause:    errBinaryDocument,
				}
			}
		}
	}
	return p.Client.Prepare(ctx, input)
}

// Send fails before dispatch, with an error wrapping llm.ErrNoAPIKey, when
// New found no API key for the public endpoint.
func (c *Client) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if c.noKey != nil {
		return llm.Result{}, c.noKey
	}
	return c.Client.Send(ctx, call)
}
