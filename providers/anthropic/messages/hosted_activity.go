package messages

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
)

// Anthropic puts results in separate blocks. The original block, including
// encrypted search data, is replayed unchanged; these fields are display only.
func (wire codec) decodeHostedUse(
	raw json.RawMessage,
	replay *llm.Opaque,
) (llm.Content, error) {
	var value struct {
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		ServerName string          `json:"server_name"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return llm.Content{}, err
	}
	use := llm.HostedUse{
		ID:         value.ID,
		Name:       value.Name,
		ServerName: value.ServerName,
		Type:       replay.Type,
		Origin:     wire.origin,
		Input:      value.Input,
	}
	if err := use.Validate(); err != nil {
		return llm.Content{}, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"invalid hosted use: %w",
			err,
		)
	}
	return llm.Content{
		Kind:      llm.ContentHostedUse,
		HostedUse: &use,
		Replay:    replay,
	}, nil
}

func (wire codec) decodeHostedResult(
	raw json.RawMessage,
	replay *llm.Opaque,
) (llm.Content, error) {
	var value struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return llm.Content{}, err
	}
	result := llm.HostedResult{
		CallID:  value.ToolUseID,
		Type:    replay.Type,
		Origin:  wire.origin,
		Output:  wire.hostedOutput(value.Content),
		IsError: value.IsError || hostedResultError(value.Content),
	}
	if err := result.Validate(); err != nil {
		return llm.Content{}, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"invalid hosted result: %w",
			err,
		)
	}
	return llm.Content{
		Kind:         llm.ContentHostedResult,
		HostedResult: &result,
		Replay:       replay,
	}, nil
}

func (wire codec) hostedOutput(raw json.RawMessage) []content.Block {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		items = []json.RawMessage{raw}
	}
	var output []content.Block
	for _, item := range items {
		var value struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			URL    string `json:"url"`
			Title  string `json:"title"`
			URI    string `json:"uri"`
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
				FileID    string `json:"file_id"`
			} `json:"source"`
			Resource struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"resource"`
		}
		if json.Unmarshal(item, &value) != nil {
			continue
		}
		if value.Text != "" {
			output = append(output, content.TextBlock(value.Text))
		}
		if value.Stdout != "" {
			output = append(output, content.TextBlock(value.Stdout))
		}
		if value.Stderr != "" {
			output = append(output, content.TextBlock(value.Stderr))
		}
		if value.Type == "image" {
			if image := wire.hostedImage(value.Source.Type,
				value.Source.MediaType, value.Source.Data,
				value.Source.URL, value.Source.FileID); image != nil {
				output = append(output, *image)
			}
		}
		if value.Resource.Text != "" {
			output = append(output, content.TextBlock(value.Resource.Text))
		}
		uri := value.URL
		if uri == "" {
			uri = value.URI
		}
		if uri == "" {
			uri = value.Resource.URI
		}
		if uri != "" {
			resource := content.Resource{
				URI:      uri,
				Title:    value.Title,
				Origin:   wire.origin,
				MIMEType: value.Resource.MIMEType,
			}
			output = append(output, content.Block{
				Kind:     content.KindResource,
				Resource: &resource,
			})
		}
	}
	return output
}

func (wire codec) hostedImage(
	sourceType string,
	mimeType string,
	encoded string,
	url string,
	fileID string,
) *content.Block {
	source := content.Source{MIMEType: mimeType}
	switch sourceType {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil
		}
		source.Data = data
	case "url":
		source.URL = url
	case "file":
		source.ID = fileID
		source.Origin = wire.origin
	default:
		return nil
	}
	block := content.Block{
		Kind: content.KindImage,
		Image: &content.Image{
			Source: source,
		},
	}
	if block.Validate() != nil {
		return nil
	}
	return &block
}

func hostedResultError(raw json.RawMessage) bool {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			if hostedResultError(item) {
				return true
			}
		}
		return false
	}
	var value struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &value) == nil &&
		strings.HasSuffix(value.Type, "_error")
}
