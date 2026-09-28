// Meta hosted web search is adapted from Dive's tool_web_search.go.
package meta

import (
	"encoding/json"
	"errors"

	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

// SearchContextSize controls how much retrieved context reaches the model.
type SearchContextSize string

const (
	// SearchContextLow requests the smallest search context.
	SearchContextLow SearchContextSize = "low"
	// SearchContextMedium requests a balanced search context.
	SearchContextMedium SearchContextSize = "medium"
	// SearchContextHigh requests the broadest search context.
	SearchContextHigh SearchContextSize = "high"
)

// UserLocation supplies an approximate locale without requiring every field.
type UserLocation struct {
	// Country is a two-letter country code when available.
	Country string `json:"country,omitempty"`
	// Region is free-text regional context for location-sensitive queries.
	Region string `json:"region,omitempty"`
	// City narrows location-sensitive queries when known.
	City string `json:"city,omitempty"`
	// Timezone supplies an IANA time zone for time-sensitive queries.
	Timezone string `json:"timezone,omitempty"`
}

// WebSearchToolOptions controls Meta's grounding and optional result include.
type WebSearchToolOptions struct {
	// SearchContextSize trades retrieved breadth for latency and token use.
	SearchContextSize SearchContextSize `json:"search_context_size,omitempty"`
	// UserLocation biases local search without sharing precise coordinates.
	UserLocation UserLocation `json:"user_location,omitempty"`
	// IncludeResults requests the hits behind the grounded answer.
	IncludeResults bool `json:"include_results,omitempty"`
}

// NewWebSearchTool declares Meta's provider-executed search grounding.
func NewWebSearchTool(options WebSearchToolOptions) (tool.Definition, error) {
	switch options.SearchContextSize {
	case "", SearchContextLow, SearchContextMedium, SearchContextHigh:
	default:
		return tool.Definition{}, errors.New(
			"meta: search context size must be low, medium, or high",
		)
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Definition{}, err
	}
	readOnly := true
	return tool.Definition{
		Name:        "web_search",
		Kind:        tool.KindProviderHosted,
		Annotations: tool.Annotations{ReadOnly: &readOnly},
		Hosted: &tool.HostedSpec{
			Provider: "meta",
			API:      responses.API,
			Type:     "web_search",
			Options:  raw,
		},
	}, nil
}
