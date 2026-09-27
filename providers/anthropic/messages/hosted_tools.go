package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"

	"github.com/deepnoodle-ai/kernel/tool"
)

const (
	// WebSearchType is the Dive-compatible Anthropic web search tool version.
	WebSearchType = "web_search_20250305"
	// CodeExecutionType is the Dive-compatible bash/text-editor tool version.
	CodeExecutionType = "code_execution_20250825"
	// CodeExecutionLegacyType selects Anthropic's older Python sandbox.
	CodeExecutionLegacyType = "code_execution_20250522"
	// WebFetchType selects Anthropic's server-side page fetch tool.
	WebFetchType = "web_fetch_20250910"
	// MCPToolsetType declares an Anthropic-hosted remote MCP connector.
	MCPToolsetType = "mcp_toolset"
)

// UserLocation supplies approximate location hints to Anthropic web search.
type UserLocation struct {
	// Type must be "approximate" for Anthropic web search.
	Type string `json:"type"`
	// City biases location-sensitive results when known.
	City string `json:"city,omitempty"`
	// Region biases location-sensitive results when known.
	Region string `json:"region,omitempty"`
	// Country biases results toward a country when known.
	Country string `json:"country,omitempty"`
	// Timezone supplies the caller's local time context.
	Timezone string `json:"timezone,omitempty"`
}

// WebSearchToolOptions retains Dive's domain, location, and usage controls.
// Provider defaults to the public Anthropic destination.
type WebSearchToolOptions struct {
	// Provider identifies the configured Messages destination.
	Provider string
	// Type selects the wire tool version; empty selects WebSearchType.
	Type string
	// MaxUses caps searches in one request; zero selects five.
	MaxUses int
	// AllowedDomains limits retrieval to the listed domains.
	AllowedDomains []string
	// BlockedDomains removes listed domains; it excludes AllowedDomains.
	BlockedDomains []string
	// UserLocation biases results without precise coordinates.
	UserLocation *UserLocation
}

// NewWebSearchTool returns a server-executed definition with no local schema.
// The destination validates the wire options again during preparation.
func NewWebSearchTool(opts WebSearchToolOptions) (tool.Definition, error) {
	if opts.Provider == "" {
		opts.Provider = "anthropic"
	}
	if opts.Type == "" {
		opts.Type = WebSearchType
	}
	if opts.Type != WebSearchType {
		return tool.Definition{}, errors.New(
			"messages: unsupported web search tool version",
		)
	}
	if opts.MaxUses == 0 {
		opts.MaxUses = 5
	}
	if opts.MaxUses < 0 ||
		(len(opts.AllowedDomains) > 0 && len(opts.BlockedDomains) > 0) {
		return tool.Definition{}, errors.New(
			"messages: invalid web search limits or domains",
		)
	}
	options := webSearchOptions{
		MaxUses:        opts.MaxUses,
		AllowedDomains: slices.Clone(opts.AllowedDomains),
		BlockedDomains: slices.Clone(opts.BlockedDomains),
		UserLocation:   opts.UserLocation,
	}
	if options.UserLocation != nil {
		location := *options.UserLocation
		if location.Type == "" {
			location.Type = "approximate"
		}
		if location.Type != "approximate" {
			return tool.Definition{}, errors.New(
				"messages: web search location must be approximate",
			)
		}
		options.UserLocation = &location
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Definition{}, err
	}
	return tool.Definition{
		Name: "web_search",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: opts.Provider,
			API:      API,
			Type:     opts.Type,
			Options:  raw,
		},
	}, nil
}

// NewCodeExecutionTool declares Anthropic's provider-executed sandbox. It
// never exposes an application executor.
func NewCodeExecutionTool(provider string) tool.Definition {
	if provider == "" {
		provider = "anthropic"
	}
	return tool.Definition{
		Name: "code_execution",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: provider,
			API:      API,
			Type:     CodeExecutionType,
		},
	}
}

// NewWebFetchTool declares Anthropic's server-side page fetch tool. MaxUses
// limits work performed by the provider during one request.
func NewWebFetchTool(provider string, maxUses int) (tool.Definition, error) {
	if maxUses < 0 {
		return tool.Definition{}, errors.New(
			"messages: web fetch max uses cannot be negative",
		)
	}
	if provider == "" {
		provider = "anthropic"
	}
	options, err := json.Marshal(struct {
		MaxUses int `json:"max_uses,omitempty"`
	}{MaxUses: maxUses})
	if err != nil {
		return tool.Definition{}, err
	}
	return tool.Definition{
		Name: "web_fetch",
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: provider,
			API:      API,
			Type:     WebFetchType,
			Options:  options,
		},
	}, nil
}

// MCPToolsetOptions describes an unauthenticated remote MCP server. Tokens
// are excluded because tool definitions are stored in request snapshots.
type MCPToolsetOptions struct {
	// Name is the label used by toolset and server declarations.
	Name string `json:"name"`
	// URL is the public HTTPS endpoint without query or embedded credentials.
	URL string `json:"url"`
}

// NewMCPToolset declares an unauthenticated server-side MCP connector.
func NewMCPToolset(
	provider string,
	options MCPToolsetOptions,
) (tool.Definition, error) {
	if err := validateMCPToolset(options); err != nil {
		return tool.Definition{}, err
	}
	if provider == "" {
		provider = "anthropic"
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Definition{}, err
	}
	return tool.Definition{
		Name: "mcp:" + options.Name,
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: provider,
			API:      API,
			Type:     MCPToolsetType,
			Options:  raw,
		},
	}, nil
}

type webSearchOptions struct {
	MaxUses        int           `json:"max_uses,omitempty"`
	AllowedDomains []string      `json:"allowed_domains,omitempty"`
	BlockedDomains []string      `json:"blocked_domains,omitempty"`
	UserLocation   *UserLocation `json:"user_location,omitempty"`
}

func (wire codec) hostedTool(d tool.Definition) (map[string]any, error) {
	spec := d.Hosted
	if spec.Provider != wire.origin.Provider || spec.API != API {
		return nil, wire.unsupported(
			"hosted tool %q requires its own destination", d.Name,
		)
	}
	config := map[string]any{
		"type": spec.Type,
		"name": d.Name,
	}
	switch spec.Type {
	case WebSearchType:
		if d.Name != "web_search" {
			return nil, wire.unsupported(
				"anthropic web search requires name web_search",
			)
		}
		var options webSearchOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, wire.unsupported("web search options: %v", err)
		}
		if options.MaxUses < 0 ||
			(len(options.AllowedDomains) > 0 && len(options.BlockedDomains) > 0) {
			return nil, wire.unsupported("invalid web search limits or domains")
		}
		if options.UserLocation != nil &&
			options.UserLocation.Type != "approximate" {
			return nil, wire.unsupported("web search location must be approximate")
		}
		if options.MaxUses > 0 {
			config["max_uses"] = options.MaxUses
		}
		if len(options.AllowedDomains) > 0 {
			config["allowed_domains"] = options.AllowedDomains
		}
		if len(options.BlockedDomains) > 0 {
			config["blocked_domains"] = options.BlockedDomains
		}
		if options.UserLocation != nil {
			config["user_location"] = options.UserLocation
		}
	case CodeExecutionType, CodeExecutionLegacyType:
		if d.Name != "code_execution" {
			return nil, wire.unsupported(
				"Anthropic code execution requires name code_execution",
			)
		}
		var options struct{}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, wire.unsupported("code execution options: %v", err)
		}
	case WebFetchType:
		if d.Name != "web_fetch" {
			return nil, wire.unsupported(
				"anthropic web fetch requires name web_fetch",
			)
		}
		var options struct {
			MaxUses int `json:"max_uses"`
		}
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, wire.unsupported("web fetch options: %v", err)
		}
		if options.MaxUses < 0 {
			return nil, wire.unsupported("web fetch max uses cannot be negative")
		}
		if options.MaxUses > 0 {
			config["max_uses"] = options.MaxUses
		}
	case MCPToolsetType:
		var options MCPToolsetOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, wire.unsupported("mcp toolset options: %w", err)
		}
		if err := validateMCPToolset(options); err != nil {
			return nil, wire.unsupported("mcp toolset options: %w", err)
		}
		if d.Name != "mcp:"+options.Name {
			return nil, wire.unsupported("mcp toolset name must match server")
		}
		config["mcp_server_name"] = options.Name
		delete(config, "name")
	default:
		return nil, wire.unsupported("hosted tool type %q", spec.Type)
	}
	return config, nil
}

func validateMCPToolset(options MCPToolsetOptions) error {
	if options.Name == "" {
		return errors.New("messages: mcp toolset requires a server name")
	}
	u, err := url.Parse(options.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" ||
		u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("messages: mcp toolset requires an HTTPS URL")
	}
	return nil
}

func decodeHostedOptions(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("messages: hosted options require one JSON object")
	}
	return nil
}
