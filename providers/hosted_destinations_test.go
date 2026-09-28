package providers_test

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestGrokHostedToolsUseDistinctWireOptions(t *testing.T) {
	client, err := grok.New(grok.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := grok.NewWebSearchTool(grok.WebSearchToolOptions{
		ExcludedDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	x, err := grok.NewXSearchTool(grok.XSearchToolOptions{
		AllowedXHandles: []string{"example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	collections, err := grok.NewCollectionsSearchTool(
		grok.CollectionsSearchToolOptions{
			CollectionIDs:  []string{"collection_1"},
			IncludeResults: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	image, err := grok.NewImageGenerationTool(grok.ImageGenerationToolOptions{
		Action: "generate",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Search"))},
			Tools: []tool.Definition{
				web.Definition,
				x.Definition,
				collections.Definition,
				grok.NewCodeExecutionTool(grok.CodeExecutionToolOptions{}).Definition,
				image.Definition,
			},
		},
	)
	if err != nil {
		t.Fatalf("prepare Grok hosted tools: %v", err)
	}
	var body struct {
		Tools   []map[string]any `json:"tools"`
		Include []string         `json:"include"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 5 {
		t.Fatal("Grok hosted definitions were lost")
	}
	if body.Tools[0]["filters"] == nil || body.Tools[1]["type"] != "x_search" {
		t.Fatal("Grok search controls used the wrong wire fields")
	}
	if body.Tools[3]["container"] != nil {
		t.Fatal("Grok code execution gained an OpenAI container field")
	}
	if body.Tools[4]["type"] != "image_generation" ||
		body.Tools[4]["action"] != "generate" || body.Tools[4]["size"] != nil {
		t.Fatal("Grok image tool used unsupported output controls")
	}
}

func TestMetaHostedWebSearchIncludesResults(t *testing.T) {
	client, err := meta.New(meta.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := meta.NewWebSearchTool(meta.WebSearchToolOptions{
		SearchContextSize: meta.SearchContextHigh,
		IncludeResults:    true,
		UserLocation: meta.UserLocation{
			Country: "US",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Search"))},
			Tools:    []tool.Definition{web.Definition},
		},
	)
	if err != nil {
		t.Fatalf("prepare Meta search: %v", err)
	}
	var body struct {
		Tools   []map[string]any `json:"tools"`
		Include []string         `json:"include"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Tools[0]["type"] != "web_search" {
		t.Fatal("Meta search used the wrong tool type")
	}
	if body.Include[len(body.Include)-1] != "web_search_call.results" {
		t.Fatal("Meta search results were not requested")
	}
}
