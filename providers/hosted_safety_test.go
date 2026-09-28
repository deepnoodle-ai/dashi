package providers_test

import (
	"testing"

	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/grok"
	"github.com/deepnoodle-ai/kernel/providers/meta"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestHostedConstructorsMarkOnlyKnownReadOnlyTools(t *testing.T) {
	tests := []struct {
		name     string
		makeTool func() (tool.Definition, error)
		readOnly bool
	}{
		{
			name: "Anthropic web search",
			makeTool: func() (tool.Definition, error) {
				return messages.NewWebSearchTool(messages.WebSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Anthropic web fetch",
			makeTool: func() (tool.Definition, error) {
				return messages.NewWebFetchTool("", 0)
			},
			readOnly: true,
		},
		{
			name: "Anthropic code execution",
			makeTool: func() (tool.Definition, error) {
				return messages.NewCodeExecutionTool(""), nil
			},
		},
		{
			name: "OpenAI web search",
			makeTool: func() (tool.Definition, error) {
				return responses.NewWebSearchTool(responses.WebSearchOptions{})
			},
			readOnly: true,
		},
		{
			name: "OpenAI file search",
			makeTool: func() (tool.Definition, error) {
				return responses.NewFileSearchTool(responses.FileSearchOptions{
					VectorStoreIDs: []string{"store"},
				})
			},
			readOnly: true,
		},
		{
			name: "OpenAI code interpreter",
			makeTool: func() (tool.Definition, error) {
				return responses.NewCodeInterpreterTool(
					responses.CodeInterpreterOptions{},
				)
			},
		},
		{
			name: "OpenAI image generation",
			makeTool: func() (tool.Definition, error) {
				return responses.NewImageGenerationTool(
					responses.ImageGenerationOptions{},
				)
			},
		},
		{
			name: "Gemini search",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewGoogleSearchTool(""), nil
			},
			readOnly: true,
		},
		{
			name: "Gemini URL context",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewURLContextTool(""), nil
			},
			readOnly: true,
		},
		{
			name: "Gemini file search",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewFileSearchTool("", gemini.FileSearchOptions{
					StoreNames: []string{"store"},
				})
			},
			readOnly: true,
		},
		{
			name: "Gemini code execution",
			makeTool: func() (tool.Definition, error) {
				return gemini.NewCodeExecutionTool(""), nil
			},
		},
		{
			name: "Grok web search",
			makeTool: func() (tool.Definition, error) {
				return grok.NewWebSearchTool(grok.WebSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Grok X search",
			makeTool: func() (tool.Definition, error) {
				return grok.NewXSearchTool(grok.XSearchToolOptions{})
			},
			readOnly: true,
		},
		{
			name: "Grok collections search",
			makeTool: func() (tool.Definition, error) {
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
			makeTool: func() (tool.Definition, error) {
				return grok.NewCodeExecutionTool(grok.CodeExecutionToolOptions{}), nil
			},
		},
		{
			name: "Grok image generation",
			makeTool: func() (tool.Definition, error) {
				return grok.NewImageGenerationTool(
					grok.ImageGenerationToolOptions{},
				)
			},
		},
		{
			name: "Meta web search",
			makeTool: func() (tool.Definition, error) {
				return meta.NewWebSearchTool(meta.WebSearchToolOptions{})
			},
			readOnly: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			definition, err := tc.makeTool()
			if err != nil {
				t.Fatal(err)
			}
			got := definition.Annotations.ReadOnly != nil &&
				*definition.Annotations.ReadOnly
			if got != tc.readOnly {
				t.Fatalf("read only=%t; want %t", got, tc.readOnly)
			}
		})
	}
}
