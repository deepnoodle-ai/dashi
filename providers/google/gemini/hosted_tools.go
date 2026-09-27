package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/deepnoodle-ai/kernel/tool"
)

// NewGoogleSearchTool enables Google's managed grounding search.
func NewGoogleSearchTool(provider string) tool.Definition {
	return hostedDefinition(provider, "google_search", "googleSearch", nil)
}

// NewURLContextTool enables retrieval of URLs supplied in the prompt.
func NewURLContextTool(provider string) tool.Definition {
	return hostedDefinition(provider, "url_context", "urlContext", nil)
}

// NewCodeExecutionTool enables Gemini's provider-managed code execution.
func NewCodeExecutionTool(provider string) tool.Definition {
	return hostedDefinition(provider, "code_execution", "codeExecution", nil)
}

// FileSearchOptions selects indexed stores and optional retrieval controls.
type FileSearchOptions struct {
	// StoreNames selects the indexed corpora this call can search.
	StoreNames []string `json:"fileSearchStoreNames"`
	// TopK limits returned chunks; zero uses the API default.
	TopK int `json:"topK,omitempty"`
	// MetadataFilter applies a provider-side document filter.
	MetadataFilter string `json:"metadataFilter,omitempty"`
}

// NewFileSearchTool declares Google's managed search over indexed files.
func NewFileSearchTool(
	provider string,
	options FileSearchOptions,
) (tool.Definition, error) {
	if len(options.StoreNames) == 0 || options.TopK < 0 {
		return tool.Definition{}, errors.New(
			"gemini: file search requires stores and nonnegative top K",
		)
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return tool.Definition{}, err
	}
	return hostedDefinition(provider, "file_search", "fileSearch", raw), nil
}

func hostedDefinition(
	provider string,
	name string,
	toolType string,
	options json.RawMessage,
) tool.Definition {
	if provider == "" {
		provider = "google"
	}
	return tool.Definition{
		Name: name,
		Kind: tool.KindProviderHosted,
		Hosted: &tool.HostedSpec{
			Provider: provider,
			API:      API,
			Type:     toolType,
			Options:  options,
		},
	}
}

func (wire codec) hostedTool(
	definition tool.Definition,
) (map[string]any, error) {
	spec := definition.Hosted
	if spec.Provider != wire.origin.Provider || spec.API != API {
		return nil, wire.unsupported(
			"hosted tool %q requires its own destination",
			definition.Name,
		)
	}
	config := map[string]any{}
	switch spec.Type {
	case "googleSearch":
		if definition.Name != "google_search" || len(spec.Options) > 0 {
			return nil, wire.unsupported("Google Search requires no options")
		}
		config["googleSearch"] = map[string]any{}
	case "urlContext":
		if definition.Name != "url_context" || len(spec.Options) > 0 {
			return nil, wire.unsupported("URL Context requires no options")
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
		var options FileSearchOptions
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
	out *FileSearchOptions,
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
