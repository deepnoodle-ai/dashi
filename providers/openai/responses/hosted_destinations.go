package responses

import (
	"time"

	"github.com/deepnoodle-ai/kernel/tool"
)

// Grok and Meta share the Responses envelope but not OpenAI's tool options.
func (wire codec) destinationHostedTool(
	definition tool.Definition,
) (map[string]any, []string, error) {
	switch wire.origin.Provider {
	case "grok":
		return wire.grokHostedTool(definition)
	case "meta":
		return wire.metaHostedTool(definition)
	}
	return nil, nil, wire.unsupported(
		"hosted tools for provider %q",
		wire.origin.Provider,
	)
}

func (wire codec) grokHostedTool(
	definition tool.Definition,
) (map[string]any, []string, error) {
	spec := definition.Hosted
	config := map[string]any{"type": spec.Type}
	switch spec.Type {
	case "web_search":
		if definition.Name != "web_search" {
			return nil, nil, wire.unsupported(
				"grok web search requires name web_search",
			)
		}
		var options struct {
			AllowedDomains           []string `json:"allowed_domains"`
			ExcludedDomains          []string `json:"excluded_domains"`
			EnableImageUnderstanding bool     `json:"enable_image_understanding"`
			EnableImageSearch        bool     `json:"enable_image_search"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("grok web search options: %w", err)
		}
		if len(options.AllowedDomains) > 5 || len(options.ExcludedDomains) > 5 ||
			(len(options.AllowedDomains) > 0 && len(options.ExcludedDomains) > 0) {
			return nil, nil, wire.unsupported(
				"grok web search requires at most five allowed or excluded domains",
			)
		}
		filters := map[string]any{}
		if len(options.AllowedDomains) > 0 {
			filters["allowed_domains"] = options.AllowedDomains
		}
		if len(options.ExcludedDomains) > 0 {
			filters["excluded_domains"] = options.ExcludedDomains
		}
		if len(filters) > 0 {
			config["filters"] = filters
		}
		if options.EnableImageUnderstanding {
			config["enable_image_understanding"] = true
		}
		if options.EnableImageSearch {
			config["enable_image_search"] = true
		}
	case "x_search":
		if definition.Name != "x_search" {
			return nil, nil, wire.unsupported(
				"grok X search requires name x_search",
			)
		}
		var options struct {
			AllowedXHandles          []string `json:"allowed_x_handles"`
			ExcludedXHandles         []string `json:"excluded_x_handles"`
			FromDate                 string   `json:"from_date"`
			ToDate                   string   `json:"to_date"`
			EnableImageUnderstanding bool     `json:"enable_image_understanding"`
			EnableVideoUnderstanding bool     `json:"enable_video_understanding"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("grok X search options: %w", err)
		}
		if len(options.AllowedXHandles) > 20 ||
			len(options.ExcludedXHandles) > 20 ||
			(len(options.AllowedXHandles) > 0 && len(options.ExcludedXHandles) > 0) {
			return nil, nil, wire.unsupported(
				"grok X search requires at most twenty allowed or excluded handles",
			)
		}
		for _, date := range []string{options.FromDate, options.ToDate} {
			if date == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, nil, wire.unsupported(
					"grok X search dates require YYYY-MM-DD",
				)
			}
		}
		if options.FromDate != "" && options.ToDate != "" &&
			options.FromDate > options.ToDate {
			return nil, nil, wire.unsupported(
				"grok X search from date must precede to date",
			)
		}
		if len(options.AllowedXHandles) > 0 {
			config["allowed_x_handles"] = options.AllowedXHandles
		}
		if len(options.ExcludedXHandles) > 0 {
			config["excluded_x_handles"] = options.ExcludedXHandles
		}
		if options.FromDate != "" {
			config["from_date"] = options.FromDate
		}
		if options.ToDate != "" {
			config["to_date"] = options.ToDate
		}
		if options.EnableImageUnderstanding {
			config["enable_image_understanding"] = true
		}
		if options.EnableVideoUnderstanding {
			config["enable_video_understanding"] = true
		}
	case "file_search":
		if definition.Name != "collections_search" {
			return nil, nil, wire.unsupported(
				"grok collections search requires name collections_search",
			)
		}
		var options struct {
			CollectionIDs  []string `json:"collection_ids"`
			MaxNumResults  int      `json:"max_num_results"`
			IncludeResults bool     `json:"include_results"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("grok collections options: %w", err)
		}
		if len(options.CollectionIDs) == 0 || options.MaxNumResults < 0 ||
			options.MaxNumResults > 50 {
			return nil, nil, wire.unsupported(
				"grok collections require IDs and at most fifty results",
			)
		}
		config["vector_store_ids"] = options.CollectionIDs
		if options.MaxNumResults > 0 {
			config["max_num_results"] = options.MaxNumResults
		}
		if options.IncludeResults {
			return config, []string{"file_search_call.results"}, nil
		}
	case "code_interpreter":
		if definition.Name != "code_execution" {
			return nil, nil, wire.unsupported(
				"grok code execution requires name code_execution",
			)
		}
		var options struct {
			IncludeOutputs bool `json:"include_outputs"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("grok code options: %w", err)
		}
		if options.IncludeOutputs {
			return config, []string{"code_interpreter_call.outputs"}, nil
		}
	case "image_generation":
		if definition.Name != "image_generation" {
			return nil, nil, wire.unsupported(
				"grok image generation requires name image_generation",
			)
		}
		var options struct {
			Action string `json:"action"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("grok image options: %w", err)
		}
		if options.Action != "" && options.Action != "auto" &&
			options.Action != "generate" && options.Action != "edit" {
			return nil, nil, wire.unsupported("invalid Grok image action")
		}
		if options.Action != "" {
			config["action"] = options.Action
		}
	default:
		return nil, nil, wire.unsupported("grok hosted type %q", spec.Type)
	}
	return config, nil, nil
}

func (wire codec) metaHostedTool(
	definition tool.Definition,
) (map[string]any, []string, error) {
	if definition.Name != "web_search" || definition.Hosted.Type != "web_search" {
		return nil, nil, wire.unsupported(
			"meta hosted tools require web_search",
		)
	}
	var options struct {
		SearchContextSize string `json:"search_context_size"`
		UserLocation      struct {
			Country  string `json:"country"`
			Region   string `json:"region"`
			City     string `json:"city"`
			Timezone string `json:"timezone"`
		} `json:"user_location"`
		IncludeResults bool `json:"include_results"`
	}
	if err := decodeHostedOptions(
		definition.Hosted.Options,
		&options,
	); err != nil {
		return nil, nil, wire.unsupported("meta web search options: %w", err)
	}
	if !searchContextSize(options.SearchContextSize) {
		return nil, nil, wire.unsupported(
			"meta search context size must be low, medium, or high",
		)
	}
	config := map[string]any{"type": "web_search"}
	if options.SearchContextSize != "" {
		config["search_context_size"] = options.SearchContextSize
	}
	location := map[string]any{"type": "approximate"}
	for key, value := range map[string]string{
		"country":  options.UserLocation.Country,
		"region":   options.UserLocation.Region,
		"city":     options.UserLocation.City,
		"timezone": options.UserLocation.Timezone,
	} {
		if value != "" {
			location[key] = value
		}
	}
	if len(location) > 1 {
		config["user_location"] = location
	}
	if options.IncludeResults {
		return config, []string{"web_search_call.results"}, nil
	}
	return config, nil, nil
}
