package providers_test

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Gemini hosted tools are checked in the providers/google/gemini module.
func TestProviderToolConstructorsMarkOnlyKnownReadOnlyTools(t *testing.T) {
	tests := []struct {
		name     string
		makeTool func() (tool.Tool, error)
		readOnly bool
	}{
		{
			name: "Anthropic web search",
			makeTool: func() (tool.Tool, error) {
				return messages.NewWebSearchTool(messages.WebSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Anthropic web fetch",
			makeTool: func() (tool.Tool, error) {
				return messages.NewWebFetchTool(messages.WebFetchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Anthropic code execution",
			makeTool: func() (tool.Tool, error) {
				return messages.NewCodeExecutionTool(messages.CodeExecutionToolOptions{}), nil
			},
		},
		{
			name: "OpenAI web search",
			makeTool: func() (tool.Tool, error) {
				return responses.NewWebSearchTool(responses.WebSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "OpenAI file search",
			makeTool: func() (tool.Tool, error) {
				return responses.NewFileSearchTool(responses.FileSearchToolOptions{
					VectorStoreIDs: []string{"store"},
				})
			},
			readOnly: true,
		},
		{
			name: "OpenAI code interpreter",
			makeTool: func() (tool.Tool, error) {
				return responses.NewCodeInterpreterTool(
					responses.CodeInterpreterToolOptions{},
				), nil
			},
		},
		{
			name: "OpenAI image generation",
			makeTool: func() (tool.Tool, error) {
				return responses.NewImageGenerationTool(
					responses.ImageGenerationToolOptions{},
				)
			},
		},
		{
			name: "Grok web search",
			makeTool: func() (tool.Tool, error) {
				return grok.NewWebSearchTool(grok.WebSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Grok X search",
			makeTool: func() (tool.Tool, error) {
				return grok.NewXSearchTool(grok.XSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Grok collections search",
			makeTool: func() (tool.Tool, error) {
				return grok.NewCollectionsSearchTool(
					grok.CollectionsSearchToolOptions{
						CollectionIDs: []string{"collection"},
					},
				)
			},
			readOnly: true,
		},
		{
			name: "Grok code execution",
			makeTool: func() (tool.Tool, error) {
				return grok.NewCodeExecutionTool(grok.CodeExecutionToolOptions{}), nil
			},
		},
		{
			name: "Grok image generation",
			makeTool: func() (tool.Tool, error) {
				return grok.NewImageGenerationTool(
					grok.ImageGenerationToolOptions{},
				)
			},
		},
		{
			name: "Meta web search",
			makeTool: func() (tool.Tool, error) {
				return meta.NewWebSearchTool(meta.WebSearchToolOptions{})
			},
			readOnly: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			providerTool, err := tc.makeTool()
			if err != nil {
				t.Fatal(err)
			}
			definition := providerTool.Definition
			got := definition.Annotations.ReadOnly != nil &&
				*definition.Annotations.ReadOnly
			if got != tc.readOnly {
				t.Fatalf("read only=%t; want %t", got, tc.readOnly)
			}
		})
	}
}
