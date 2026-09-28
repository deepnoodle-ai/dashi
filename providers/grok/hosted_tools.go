package grok

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

// WebSearchToolOptions controls Grok's web search domain and image features.
type WebSearchToolOptions struct {
	// AllowedDomains limits retrieval to at most five domains.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// ExcludedDomains removes at most five domains; it excludes AllowedDomains.
	ExcludedDomains []string `json:"excluded_domains,omitempty"`
	// EnableImageUnderstanding lets Grok inspect images it finds.
	EnableImageUnderstanding bool `json:"enable_image_understanding,omitempty"`
	// EnableImageSearch lets Grok search for images separately.
	EnableImageSearch bool `json:"enable_image_search,omitempty"`
}

// NewWebSearchTool declares Grok's provider-executed web search.
func NewWebSearchTool(options WebSearchToolOptions) (tool.Definition, error) {
	if len(options.AllowedDomains) > 5 || len(options.ExcludedDomains) > 5 ||
		(len(options.AllowedDomains) > 0 && len(options.ExcludedDomains) > 0) {
		return tool.Definition{}, errors.New(
			"grok: web search requires at most five allowed or excluded domains",
		)
	}
	return newHosted("web_search", "web_search", options)
}

// XSearchToolOptions controls search over X posts and media.
type XSearchToolOptions struct {
	// AllowedXHandles limits posts to at most twenty accounts.
	AllowedXHandles []string `json:"allowed_x_handles,omitempty"`
	// ExcludedXHandles removes at most twenty accounts; it excludes allowed
	// handles.
	ExcludedXHandles []string `json:"excluded_x_handles,omitempty"`
	// FromDate is the inclusive YYYY-MM-DD lower bound.
	FromDate string `json:"from_date,omitempty"`
	// ToDate is the YYYY-MM-DD upper bound.
	ToDate string `json:"to_date,omitempty"`
	// EnableImageUnderstanding lets Grok inspect images in posts.
	EnableImageUnderstanding bool `json:"enable_image_understanding,omitempty"`
	// EnableVideoUnderstanding lets Grok inspect videos in posts.
	EnableVideoUnderstanding bool `json:"enable_video_understanding,omitempty"`
}

// NewXSearchTool declares Grok's provider-executed X search.
func NewXSearchTool(options XSearchToolOptions) (tool.Definition, error) {
	if len(options.AllowedXHandles) > 20 ||
		len(options.ExcludedXHandles) > 20 ||
		(len(options.AllowedXHandles) > 0 && len(options.ExcludedXHandles) > 0) {
		return tool.Definition{}, errors.New(
			"grok: X search requires at most twenty allowed or excluded handles",
		)
	}
	for _, date := range []string{options.FromDate, options.ToDate} {
		if date == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return tool.Definition{}, errors.New(
				"grok: X search dates require YYYY-MM-DD",
			)
		}
	}
	if options.FromDate != "" && options.ToDate != "" &&
		options.FromDate > options.ToDate {
		return tool.Definition{}, errors.New(
			"grok: X search from date must precede to date",
		)
	}
	return newHosted("x_search", "x_search", options)
}

// CollectionsSearchToolOptions identifies xAI knowledge collections.
type CollectionsSearchToolOptions struct {
	// CollectionIDs selects at least one xAI knowledge collection.
	CollectionIDs []string `json:"collection_ids"`
	// MaxNumResults caps matches at fifty; zero keeps the API default.
	MaxNumResults int `json:"max_num_results,omitempty"`
	// IncludeResults asks the API to return matched chunks for inspection.
	IncludeResults bool `json:"include_results,omitempty"`
}

// NewCollectionsSearchTool maps Grok collections to file_search on the wire.
func NewCollectionsSearchTool(
	options CollectionsSearchToolOptions,
) (tool.Definition, error) {
	if len(options.CollectionIDs) == 0 ||
		options.MaxNumResults < 0 || options.MaxNumResults > 50 {
		return tool.Definition{}, errors.New(
			"grok: collections search requires IDs and at most fifty results",
		)
	}
	return newHosted("collections_search", "file_search", options)
}

// CodeExecutionToolOptions controls return of Grok's provider sandbox output.
type CodeExecutionToolOptions struct {
	// IncludeOutputs requests logs and files returned by the sandbox.
	IncludeOutputs bool `json:"include_outputs,omitempty"`
}

// NewCodeExecutionTool declares Grok's hosted Python sandbox.
func NewCodeExecutionTool(options CodeExecutionToolOptions) tool.Definition {
	raw := json.RawMessage(`{}`)
	if options.IncludeOutputs {
		raw = json.RawMessage(`{"include_outputs":true}`)
	}
	return tool.Definition{
		Name: "code_execution",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: "grok",
			API:      responses.API,
			Type:     "code_interpreter",
			Options:  raw,
		},
	}
}

// ImageGenerationToolOptions controls whether Grok may generate, edit, or
// choose between the two. The hosted tool chooses its own size and format.
type ImageGenerationToolOptions struct {
	Action string `json:"action,omitempty"`
}

// NewImageGenerationTool lets Grok generate or edit images within a response.
func NewImageGenerationTool(
	options ImageGenerationToolOptions,
) (tool.Definition, error) {
	if options.Action != "" && options.Action != "auto" &&
		options.Action != "generate" && options.Action != "edit" {
		return tool.Definition{}, errors.New("grok: image action must be auto, generate, or edit")
	}
	return newHosted("image_generation", "image_generation", options)
}

func newHosted(name, toolType string, options any) (tool.Definition, error) {
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Definition{}, err
	}
	definition := tool.Definition{
		Name: name,
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: "grok",
			API:      responses.API,
			Type:     toolType,
			Options:  raw,
		},
	}
	if toolType == "web_search" || toolType == "x_search" ||
		toolType == "file_search" {
		definition.Annotations.ReadOnly = new(true)
	}
	return definition, nil
}
