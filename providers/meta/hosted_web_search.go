package meta

import (
	"encoding/json"
	"errors"

	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
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
	UserLocation UserLocation `json:"user_location,omitzero"`
	// IncludeResults requests the hits behind the grounded answer.
	IncludeResults bool `json:"include_results,omitempty"`
}

// NewWebSearchTool declares Meta's provider-executed search grounding. It
// returns a definition-only tool.Tool: Meta runs it inside the model call, so
// it has no Executor and needs no Policy. Add it to a Runner with tool.List,
// or pass its Definition in llm.Request.Tools. The tool is bound to the
// destination named "meta" that New configures.
func NewWebSearchTool(options WebSearchToolOptions) (tool.Tool, error) {
	switch options.SearchContextSize {
	case "", SearchContextLow, SearchContextMedium, SearchContextHigh:
	default:
		return tool.Tool{}, errors.New(
			"meta: search context size must be low, medium, or high",
		)
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Tool{}, err
	}
	return tool.Tool{Definition: tool.Definition{
		Name:        "web_search",
		Kind:        tool.KindProviderHosted,
		Annotations: tool.Annotations{ReadOnly: new(true)},
		Hosted: &tool.HostedSpec{
			Provider: "meta",
			API:      responses.API,
			Type:     "web_search",
			Options:  raw,
		},
	}}, nil
}
