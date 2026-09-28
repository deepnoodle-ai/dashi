package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/deepnoodle-ai/dashi/tool"
)

// GoogleSearchToolOptions binds Google Search grounding to a destination.
type GoogleSearchToolOptions struct {
	// Provider names the configured destination; empty means "google",
	// which a Vertex client with the default provider name also accepts.
	Provider string
}

// NewGoogleSearchTool enables Google's managed grounding search.
func NewGoogleSearchTool(options GoogleSearchToolOptions) tool.Tool {
	return newHosted(options.Provider, "google_search", "googleSearch", nil)
}

// URLContextToolOptions binds URL context retrieval to a destination.
type URLContextToolOptions struct {
	// Provider names the configured destination; empty means "google",
	// which a Vertex client with the default provider name also accepts.
	Provider string
}

// NewURLContextTool enables retrieval of URLs supplied in the prompt.
func NewURLContextTool(options URLContextToolOptions) tool.Tool {
	return newHosted(options.Provider, "url_context", "urlContext", nil)
}

// CodeExecutionToolOptions binds Gemini code execution to a destination.
type CodeExecutionToolOptions struct {
	// Provider names the configured destination; empty means "google",
	// which a Vertex client with the default provider name also accepts.
	Provider string
}

// NewCodeExecutionTool enables Gemini's provider-managed code execution.
func NewCodeExecutionTool(options CodeExecutionToolOptions) tool.Tool {
	return newHosted(options.Provider, "code_execution", "codeExecution", nil)
}

// FileSearchToolOptions selects indexed stores and optional retrieval
// controls.
type FileSearchToolOptions struct {
	// Provider names the configured destination; empty means "google". File
	// search stores belong to one destination, so this binding is exact.
	Provider string `json:"-"`
	// StoreNames selects the indexed corpora this call can search; at least
	// one is required.
	StoreNames []string `json:"fileSearchStoreNames"`
	// TopK limits returned chunks; zero uses the API default.
	TopK int `json:"topK,omitempty"`
	// MetadataFilter applies a provider-side document filter.
	MetadataFilter string `json:"metadataFilter,omitempty"`
}

// NewFileSearchTool declares Google's managed search over indexed files.
func NewFileSearchTool(options FileSearchToolOptions) (tool.Tool, error) {
	if len(options.StoreNames) == 0 || options.TopK < 0 {
		return tool.Tool{}, errors.New(
			"gemini: file search requires stores and nonnegative top K",
		)
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Tool{}, err
	}
	return newHosted(options.Provider, "file_search", "fileSearch", raw), nil
}

func newHosted(
	provider string,
	name string,
	toolType string,
	options json.RawMessage,
) tool.Tool {
	if provider == "" {
		provider = "google"
	}
	definition := tool.Definition{
		Name: name,
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: provider,
			API:      API,
			Type:     toolType,
			Options:  options,
		},
	}
	if toolType == "googleSearch" || toolType == "urlContext" ||
		toolType == "fileSearch" {
		definition.Annotations.ReadOnly = new(true)
	}
	return tool.Tool{Definition: definition}
}

// boundTo reports whether spec may be sent to the destination named
// provider. Binding is exact, except that an option-free tool bound to the
// default "google" destination also serves "google-vertex": it carries no
// destination-specific data, such as file search stores.
func boundTo(spec *tool.HostedSpec, provider string) bool {
	if spec.Provider == provider {
		return true
	}
	if spec.Provider != "google" || provider != "google-vertex" ||
		len(spec.Options) > 0 {
		return false
	}
	switch spec.Type {
	case "googleSearch", "urlContext", "codeExecution":
		return true
	}
	return false
}

func (wire codec) hostedTool(
	definition tool.Definition,
) (map[string]any, error) {
	spec := definition.Hosted
	if !boundTo(spec, wire.origin.Provider) || spec.API != API {
		return nil, wire.unsupported(
			"hosted tool %q requires its own destination",
			definition.Name,
		)
	}
	config := map[string]any{}
	switch spec.Type {
	case "googleSearch":
		if definition.Name != "google_search" || len(spec.Options) > 0 {
			return nil, wire.unsupported("google search requires no options")
		}
		config["googleSearch"] = map[string]any{}
	case "urlContext":
		if definition.Name != "url_context" || len(spec.Options) > 0 {
			return nil, wire.unsupported("url context requires no options")
		}
		config["urlContext"] = map[string]any{}
	case "codeExecution":
		if definition.Name != "code_execution" || len(spec.Options) > 0 {
			return nil, wire.unsupported("code execution requires no options")
		}
		config["codeExecution"] = map[string]any{}
	case "fileSearch":
		if definition.Name != "file_search" {
			return nil, wire.unsupported("file search requires name file_search")
		}
		var options FileSearchToolOptions
		if err := decodeFileSearchOptions(spec.Options, &options); err != nil ||
			len(options.StoreNames) == 0 || options.TopK < 0 {
			return nil, wire.unsupported("file search requires valid stores and top K")
		}
		config["fileSearch"] = options
	default:
		return nil, wire.unsupported("hosted tool type %q", spec.Type)
	}
	return config, nil
}

func decodeFileSearchOptions(
	raw json.RawMessage,
	out *FileSearchToolOptions,
) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("gemini: file search requires one options object")
	}
	return nil
}
