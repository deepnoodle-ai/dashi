package responses

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/internal/providerhttp"
	"github.com/deepnoodle-ai/kernel/llm"
)

// A Responses hosted item carries its result in the same item. Its public
// fields are for display; replay preserves the complete provider item.
func (wire codec) decodeHostedItem(
	raw json.RawMessage,
	replay *llm.Opaque,
) ([]llm.Content, error) {
	var value struct {
		ID            string            `json:"id"`
		Status        string            `json:"status"`
		Name          string            `json:"name"`
		ServerLabel   string            `json:"server_label"`
		Arguments     string            `json:"arguments"`
		Action        json.RawMessage   `json:"action"`
		Queries       json.RawMessage   `json:"queries"`
		Code          string            `json:"code"`
		RevisedPrompt string            `json:"revised_prompt"`
		Prompt        string            `json:"prompt"`
		Results       []json.RawMessage `json:"results"`
		Outputs       []json.RawMessage `json:"outputs"`
		Result        string            `json:"result"`
		Output        string            `json:"output"`
		Error         json.RawMessage   `json:"error"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(replay.Type, "_call")
	if value.Name != "" {
		name = value.Name
	}
	input := value.Action
	switch replay.Type {
	case "file_search_call":
		input = value.Queries
	case "code_interpreter_call":
		input, _ = json.Marshal(map[string]string{"code": value.Code})
	case "image_generation_call":
		if value.Prompt != "" {
			input, _ = json.Marshal(map[string]string{"prompt": value.Prompt})
		} else {
			input, _ = json.Marshal(map[string]string{
				"revised_prompt": value.RevisedPrompt,
			})
		}
	case "mcp_call":
		input = json.RawMessage(value.Arguments)
		if !json.Valid(input) {
			replay.Partial = true
			return []llm.Content{{Kind: llm.ContentOpaque, Opaque: replay}}, nil
		}
	}
	status := value.Status
	if status == "" {
		status = "completed"
	}
	use := llm.HostedUse{
		ID:         value.ID,
		Name:       name,
		ServerName: value.ServerLabel,
		Type:       replay.Type,
		Origin:     wire.origin,
		Status:     status,
		Input:      input,
		Output:     wire.hostedItemOutput(value.Results, value.Outputs),
		IsError:    status == "failed" || nonNull(value.Error),
	}
	if replay.Type == "web_search_call" {
		use.Output = append(use.Output, wire.webSearchSources(value.Action)...)
	}
	if value.Output != "" {
		use.Output = append(use.Output, content.TextBlock(value.Output))
	}
	if value.Result != "" {
		data, err := base64.StdEncoding.DecodeString(value.Result)
		if err == nil {
			mimeType := http.DetectContentType(data)
			block := content.Block{
				Kind: content.KindImage,
				Image: &content.Image{
					Source: content.Source{
						MIMEType: mimeType,
						Data:     data,
					},
				},
			}
			use.Output = append(use.Output, block)
		}
	}
	if err := use.Validate(); err != nil {
		return nil, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"invalid hosted item: %w",
			err,
		)
	}
	return []llm.Content{
		{
			Kind:      llm.ContentHostedUse,
			HostedUse: &use,
			Replay:    replay,
		},
	}, nil
}

func (wire codec) decodeHostedApproval(
	raw json.RawMessage,
	replay *llm.Opaque,
) ([]llm.Content, error) {
	var value struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		ServerLabel string `json:"server_label"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	request := llm.HostedApprovalRequest{
		ID:         value.ID,
		Name:       value.Name,
		ServerName: value.ServerLabel,
		Arguments:  json.RawMessage(value.Arguments),
		Origin:     wire.origin,
	}
	if err := request.Validate(); err != nil {
		return nil, providerhttp.Failure(
			wire.origin.Provider,
			llm.ErrorProtocol,
			"invalid hosted approval: %w",
			err,
		)
	}
	return []llm.Content{
		{
			Kind:                  llm.ContentHostedApprovalRequest,
			HostedApprovalRequest: &request,
			Replay:                replay,
		},
	}, nil
}

func (wire codec) hostedItemOutput(
	results []json.RawMessage,
	outputs []json.RawMessage,
) []content.Block {
	var blocks []content.Block
	for _, raw := range slices.Concat(results, outputs) {
		var item struct {
			URL      string `json:"url"`
			Title    string `json:"title"`
			Text     string `json:"text"`
			Snippet  string `json:"snippet"`
			Logs     string `json:"logs"`
			FileID   string `json:"file_id"`
			Filename string `json:"filename"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		for _, value := range []string{item.Text, item.Snippet, item.Logs} {
			if value != "" {
				blocks = append(blocks, content.TextBlock(value))
			}
		}
		if item.URL != "" {
			resource := content.Resource{
				URI:    item.URL,
				Title:  item.Title,
				Origin: wire.origin,
			}
			blocks = append(
				blocks,
				content.Block{
					Kind:     content.KindResource,
					Resource: &resource,
				},
			)
		}
		if item.FileID != "" {
			blocks = append(
				blocks,
				content.Block{
					Kind: content.KindDocument,
					Document: &content.Document{
						Title: item.Filename,
						Source: content.Source{
							ID:     item.FileID,
							Origin: wire.origin,
						},
					},
				},
			)
		}
	}
	return blocks
}

func (wire codec) webSearchSources(action json.RawMessage) []content.Block {
	var value struct {
		Sources []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"sources"`
	}
	if json.Unmarshal(action, &value) != nil {
		return nil
	}
	var blocks []content.Block
	for _, source := range value.Sources {
		if source.URL == "" {
			continue
		}
		resource := content.Resource{
			URI:    source.URL,
			Title:  source.Title,
			Origin: wire.origin,
		}
		blocks = append(
			blocks,
			content.Block{
				Kind:     content.KindResource,
				Resource: &resource,
			},
		)
	}
	return blocks
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
