package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"

	"github.com/deepnoodle-ai/dashi/tool"
)

// Hosted tool constructors return a definition-only tool.Tool: OpenAI runs it
// inside the model call, so it has no Executor and needs no Policy. Add it to
// a Runner with tool.List, or pass its Definition in llm.Request.Tools. A
// constructor returns an error only when its options can be invalid. These
// tools are bound to the destination named "openai", the default
// Config.Provider; destinations with other names do not accept them.

// WebSearchToolOptions configures OpenAI's web search tool.
type WebSearchToolOptions struct {
	// AllowedDomains narrows search to these domains when using web_search.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// SearchContextSize is "low", "medium", or "high"; empty uses the
	// provider default. It trades returned context for latency and token use.
	SearchContextSize string `json:"search_context_size,omitempty"`
}

// NewWebSearchTool declares OpenAI's provider-executed web search.
func NewWebSearchTool(options WebSearchToolOptions) (tool.Tool, error) {
	if !searchContextSize(options.SearchContextSize) {
		return tool.Tool{}, errors.New("responses: invalid search context size")
	}
	return hostedTool("web_search", "web_search", options), nil
}

// FileSearchToolOptions identifies the vector stores available to OpenAI
// search.
type FileSearchToolOptions struct {
	// VectorStoreIDs selects the stores this call may query; at least one is
	// required.
	VectorStoreIDs []string `json:"vector_store_ids"`
	// MaxNumResults caps matched chunks; zero accepts the provider default.
	MaxNumResults int `json:"max_num_results,omitempty"`
	// IncludeResults asks the API to return matched chunks for inspection.
	IncludeResults bool `json:"include_results,omitempty"`
}

// NewFileSearchTool declares provider-managed vector-store search.
func NewFileSearchTool(options FileSearchToolOptions) (tool.Tool, error) {
	if len(options.VectorStoreIDs) == 0 || options.MaxNumResults < 0 {
		return tool.Tool{}, errors.New(
			"responses: file search requires stores and nonnegative limit",
		)
	}
	return hostedTool("file_search", "file_search", options), nil
}

// CodeInterpreterToolOptions supplies files to the provider's code container.
type CodeInterpreterToolOptions struct {
	// FileIDs makes provider-managed files available inside the container.
	FileIDs []string `json:"file_ids,omitempty"`
}

// NewCodeInterpreterTool declares OpenAI's provider-managed code container.
func NewCodeInterpreterTool(options CodeInterpreterToolOptions) tool.Tool {
	return hostedTool("code_interpreter", "code_interpreter", options)
}

// ImageGenerationToolOptions configures the provider-hosted image generator.
type ImageGenerationToolOptions struct {
	// Model selects a GPT Image model independently of the mainline model.
	Model string `json:"model,omitempty"`
	// Size selects a provider-supported image dimension.
	Size string `json:"size,omitempty"`
	// Quality selects a provider-supported quality level.
	Quality string `json:"quality,omitempty"`
	// Background selects transparent or opaque generation where supported.
	Background string `json:"background,omitempty"`
	// OutputFormat selects the returned image encoding.
	OutputFormat string `json:"output_format,omitempty"`
	// OutputCompression is a percentage from 0 to 100 for JPEG or WebP. A
	// pointer preserves an explicit zero, which means no compression.
	OutputCompression *int `json:"output_compression,omitempty"`
	// Action is "auto", "generate", or "edit"; empty uses the provider
	// default.
	Action string `json:"action,omitempty"`
}

// NewImageGenerationTool declares OpenAI's provider-hosted image generator.
func NewImageGenerationTool(
	options ImageGenerationToolOptions,
) (tool.Tool, error) {
	if err := validateImageGeneration(options); err != nil {
		return tool.Tool{}, errors.New("responses: " + err.Error())
	}
	return hostedTool("image_generation", "image_generation", options), nil
}

// MCPToolOptions connects an unauthenticated remote MCP server. Tokens and
// custom headers are deliberately excluded from the recordable tool
// definition.
type MCPToolOptions struct {
	// ServerLabel names the remote server in the provider's response.
	ServerLabel string `json:"server_label"`
	// ServerURL must be HTTPS and contain no embedded credentials or query.
	ServerURL string `json:"server_url"`
	// ServerDescription supplies context for selecting remote tools.
	ServerDescription string `json:"server_description,omitempty"`
	// AllowedTools restricts the remote tool names available to the model.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// RequireApproval is "always" or "never"; empty uses the API default.
	RequireApproval string `json:"require_approval,omitempty"`
}

// NewMCPTool declares an unauthenticated remote server. Approval defaults to
// the provider's ask-every-time behavior; this constructor never sets tokens.
func NewMCPTool(options MCPToolOptions) (tool.Tool, error) {
	if err := validateMCPOptions(options); err != nil {
		return tool.Tool{}, err
	}
	return hostedTool("mcp:"+options.ServerLabel, "mcp", options), nil
}

// hostedTool binds options to the "openai" destination. Options are plain
// structs of strings, numbers, and slices, so encoding cannot fail.
func hostedTool(name, toolType string, options any) tool.Tool {
	raw, err := json.Marshal(options)
	if err != nil {
		panic("responses: encode hosted tool options: " + err.Error())
	}
	definition := tool.Definition{
		Name: name,
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: "openai",
			API:      API,
			Type:     toolType,
			Options:  raw,
		},
	}
	if toolType == "web_search" || toolType == "file_search" {
		definition.Annotations.ReadOnly = new(true)
	}
	return tool.Tool{Definition: definition}
}

func (wire codec) hostedTool(
	definition tool.Definition,
) (map[string]any, []string, error) {
	spec := definition.Hosted
	if spec.Provider != wire.origin.Provider || spec.API != API {
		return nil, nil, wire.unsupported(
			"hosted tool %q requires its own destination",
			definition.Name,
		)
	}
	if spec.Provider != "openai" {
		return wire.destinationHostedTool(definition)
	}
	config := map[string]any{"type": spec.Type}
	var includes []string
	switch spec.Type {
	case "web_search", "web_search_preview":
		if definition.Name != "web_search" {
			return nil, nil, wire.unsupported("web search requires name web_search")
		}
		var options WebSearchToolOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("web search options: %v", err)
		}
		if !searchContextSize(options.SearchContextSize) {
			return nil, nil, wire.unsupported("invalid search context size")
		}
		if options.SearchContextSize != "" {
			config["search_context_size"] = options.SearchContextSize
		}
		if len(options.AllowedDomains) > 0 {
			if spec.Type == "web_search_preview" {
				return nil, nil, wire.unsupported("preview web search domain filter")
			}
			config["filters"] = map[string]any{
				"allowed_domains": slices.Clone(options.AllowedDomains),
			}
		}
		includes = append(includes, "web_search_call.action.sources")
	case "file_search":
		if definition.Name != "file_search" {
			return nil, nil, wire.unsupported("file search requires name file_search")
		}
		var options FileSearchToolOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("file search options: %v", err)
		}
		if len(options.VectorStoreIDs) == 0 || options.MaxNumResults < 0 {
			return nil, nil, wire.unsupported(
				"file search requires stores and nonnegative limit",
			)
		}
		config["vector_store_ids"] = slices.Clone(options.VectorStoreIDs)
		if options.MaxNumResults > 0 {
			config["max_num_results"] = options.MaxNumResults
		}
		if options.IncludeResults {
			includes = append(includes, "file_search_call.results")
		}
	case "code_interpreter":
		if definition.Name != "code_interpreter" {
			return nil, nil, wire.unsupported(
				"code interpreter requires name code_interpreter",
			)
		}
		var options CodeInterpreterToolOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("code interpreter options: %v", err)
		}
		container := map[string]any{"type": "auto"}
		if len(options.FileIDs) > 0 {
			container["file_ids"] = slices.Clone(options.FileIDs)
		}
		config["container"] = container
		includes = append(includes, "code_interpreter_call.outputs")
	case "image_generation":
		if definition.Name != "image_generation" {
			return nil, nil, wire.unsupported(
				"image generation requires name image_generation",
			)
		}
		var options ImageGenerationToolOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("image generation options: %v", err)
		}
		if err := validateImageGeneration(options); err != nil {
			return nil, nil, wire.unsupported("%v", err)
		}
		if options.OutputCompression != nil {
			config["output_compression"] = *options.OutputCompression
		}
		for key, value := range map[string]string{
			"model":         options.Model,
			"size":          options.Size,
			"quality":       options.Quality,
			"background":    options.Background,
			"output_format": options.OutputFormat,
			"action":        options.Action,
		} {
			if value != "" {
				config[key] = value
			}
		}
	case "mcp":
		var options MCPToolOptions
		if err := decodeHostedOptions(spec.Options, &options); err != nil {
			return nil, nil, wire.unsupported("mcp options: %w", err)
		}
		if definition.Name != "mcp:"+options.ServerLabel {
			return nil, nil, wire.unsupported(
				"mcp tool name must match server label",
			)
		}
		if err := validateMCPOptions(options); err != nil {
			return nil, nil, wire.unsupported("mcp options: %w", err)
		}
		config["server_label"] = options.ServerLabel
		config["server_url"] = options.ServerURL
		if options.ServerDescription != "" {
			config["server_description"] = options.ServerDescription
		}
		if len(options.AllowedTools) > 0 {
			config["allowed_tools"] = slices.Clone(options.AllowedTools)
		}
		if options.RequireApproval != "" {
			config["require_approval"] = options.RequireApproval
		}
	default:
		return nil, nil, wire.unsupported("hosted tool type %q", spec.Type)
	}
	return config, includes, nil
}

func validateMCPOptions(options MCPToolOptions) error {
	if options.ServerLabel == "" {
		return errors.New("responses: mcp requires a server label")
	}
	u, err := url.Parse(options.ServerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" ||
		u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return errors.New("responses: mcp requires an HTTPS server URL")
	}
	switch options.RequireApproval {
	case "", "always", "never":
		return nil
	}
	return errors.New("responses: mcp approval must be always or never")
}

func validateImageGeneration(options ImageGenerationToolOptions) error {
	if options.Action != "" && options.Action != "auto" &&
		options.Action != "generate" && options.Action != "edit" {
		return fmt.Errorf("image generation action %q", options.Action)
	}
	if options.OutputCompression != nil &&
		(*options.OutputCompression < 0 || *options.OutputCompression > 100 ||
			(options.OutputFormat != "jpeg" && options.OutputFormat != "webp")) {
		return errors.New("image compression requires JPEG or WebP and a value from 0 to 100")
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
		return errors.New("responses: hosted options require one JSON object")
	}
	return nil
}

func searchContextSize(value string) bool {
	switch value {
	case "", "low", "medium", "high":
		return true
	}
	return false
}
