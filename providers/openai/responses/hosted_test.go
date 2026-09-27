package responses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestHostedItemsReplayWithoutExecution(t *testing.T) {
	client, err := New(Config{
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := codec{origin: client.client.Origin("test-model")}
	raw := []byte(`{
		"id":"resp_1","object":"response","model":"test-model",
		"status":"completed","output":[
			{"type":"web_search_call","id":"ws_1","status":"completed",
			 "action":{"type":"search","query":"kernel"},
			 "results":[{"url":"https://example.com","title":"Example","snippet":"Found"}]},
			{"type":"message","id":"msg_1","role":"assistant",
			 "content":[{"type":"output_text","text":"Answer"}]}
		]
	}`)
	result, err := wire.decode(raw, llm.ModelRef{
		Provider: "openai",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatalf("decode hosted output: %v", err)
	}
	if len(result.Message.Content) != 2 {
		t.Fatal("hosted output or answer was dropped")
	}
	use := result.Message.Content[0].HostedUse
	if use == nil || use.Name != "web_search" {
		t.Fatal("hosted search became an executable client call")
	}
	if len(use.Output) != 2 || use.Output[1].Resource == nil {
		t.Fatal("search snippet or resource reference was lost")
	}
	request, err := client.Prepare(context.Background(), llm.Input{
		Model: "test-model",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Search")),
			result.Message,
			llm.UserMessage(llm.Text("Thanks")),
		},
	})
	if err != nil {
		t.Fatalf("prepare hosted history: %v", err)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 4 {
		t.Fatal("hosted item or message order changed")
	}
	var item struct {
		Results []struct {
			URL string `json:"url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body.Input[1], &item); err != nil {
		t.Fatal(err)
	}
	if item.Results[0].URL != "https://example.com" {
		t.Fatal("hosted provider item was not replayed")
	}
}

func TestHostedDefinitionsRequestResults(t *testing.T) {
	client, err := New(Config{
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := NewWebSearchTool(WebSearchOptions{
		AllowedDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := NewFileSearchTool(FileSearchOptions{
		VectorStoreIDs: []string{"vs_1"},
		IncludeResults: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := NewCodeInterpreterTool(CodeInterpreterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	image, err := NewImageGenerationTool(ImageGenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(context.Background(), llm.Input{
		Model:    "test-model",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Research"))},
		Tools:    []tool.Definition{web, files, code, image},
	})
	if err != nil {
		t.Fatalf("prepare hosted tools: %v", err)
	}
	var body struct {
		Tools   []map[string]any `json:"tools"`
		Include []string         `json:"include"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 4 || len(body.Include) != 4 {
		t.Fatal("hosted tools or requested result includes were omitted")
	}
	if body.Tools[1]["vector_store_ids"] == nil {
		t.Fatal("file search lost its vector store")
	}
}

func TestMCPApprovalRequiresExplicitResponse(t *testing.T) {
	client, err := New(Config{
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := NewMCPTool(MCPOptions{
		ServerLabel: "docs",
		ServerURL:   "https://example.com/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := codec{origin: client.client.Origin("test-model")}
	items, err := wire.decodeItem(json.RawMessage(`{
		"type":"mcp_approval_request","id":"approval_1",
		"server_label":"docs","name":"read","arguments":"{}"
	}`))
	if err != nil || len(items) != 1 || items[0].HostedApprovalRequest == nil {
		t.Fatal("MCP approval request was not exposed", err)
	}
	input := llm.Input{
		Model: "test-model",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Read the docs")),
			llm.AssistantMessage(items[0]),
		},
		Tools: []tool.Definition{definition},
	}
	if _, err := client.Prepare(context.Background(), input); err != nil {
		t.Fatalf("pending approval should replay without a decision: %v", err)
	}
	response := llm.Content{
		Kind: llm.ContentHostedApprovalResponse,
		HostedApprovalResponse: &llm.HostedApprovalResponse{
			RequestID: "approval_1",
			Approve:   false,
			Reason:    "Denied by caller",
			Origin:    wire.origin,
		},
	}
	input.Messages = append(input.Messages, llm.UserMessage(response))
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatalf("prepare explicit denial: %v", err)
	}
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	last := body.Input[len(body.Input)-1]
	if last["type"] != "mcp_approval_response" || last["approve"] != false {
		t.Fatal("explicit MCP denial was not encoded")
	}
}

func TestMCPOptionsRejectEmbeddedCredentials(t *testing.T) {
	definition, err := NewMCPTool(MCPOptions{
		ServerLabel: "docs",
		ServerURL:   "https://example.com/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition.Hosted.Options = json.RawMessage(`{
		"server_label":"docs","server_url":"https://example.com/mcp",
		"authorization":"secret"
	}`)
	client, err := New(Config{
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(context.Background(), llm.Input{
		Model:    "test-model",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Read"))},
		Tools:    []tool.Definition{definition},
	})
	if err == nil {
		t.Fatal("recordable MCP definition accepted an embedded token")
	}
}

func TestHostedImageAndMCPResultsRemainObservable(t *testing.T) {
	wire := codec{origin: content.Origin{
		Provider: "openai",
		API:      API,
		Model:    "test-model",
		Scope:    "test-scope",
	}}
	imageBytes := []byte("\x89PNG\r\n\x1a\n")
	raw := json.RawMessage(`{
		"type":"image_generation_call","id":"image_1",
		"status":"completed","result":"` +
		base64.StdEncoding.EncodeToString(imageBytes) + `"
	}`)
	items, err := wire.decodeItem(raw)
	if err != nil {
		t.Fatalf("decode generated image: %v", err)
	}
	if len(items) != 1 || items[0].HostedUse == nil ||
		len(items[0].HostedUse.Output) != 1 {
		t.Fatal("generated image was dropped")
	}
	image := items[0].HostedUse.Output[0].Image
	if image == nil || image.Source.MIMEType != "image/png" {
		t.Fatal("generated image MIME type was lost")
	}
	items, err = wire.decodeItem(json.RawMessage(`{
		"type":"mcp_call","id":"mcp_1","name":"lookup",
		"server_label":"docs","arguments":"{}","output":"Result",
		"status":"completed"
	}`))
	if err != nil {
		t.Fatalf("decode MCP call: %v", err)
	}
	use := items[0].HostedUse
	if use == nil || use.ServerName != "docs" ||
		len(use.Output) != 1 || use.Output[0].Text.Text != "Result" {
		t.Fatal("MCP result lost its server or output")
	}
}

func TestFileSearchResultsKeepProviderFileID(t *testing.T) {
	wire := codec{origin: content.Origin{
		Provider: "openai",
		API:      API,
		Model:    "test-model",
		Scope:    "test-scope",
	}}
	items, err := wire.decodeItem(json.RawMessage(`{
		"type":"file_search_call","id":"fs_1","status":"completed",
		"queries":["kernel"],"results":[{
			"file_id":"file_1","filename":"guide.pdf","text":"Relevant passage"
		}]
	}`))
	if err != nil {
		t.Fatalf("decode file search: %v", err)
	}
	output := items[0].HostedUse.Output
	if len(output) != 2 || output[0].Text == nil ||
		output[1].Document == nil ||
		output[1].Document.Source.ID != "file_1" ||
		output[1].Document.Source.Origin != wire.origin {
		t.Fatal("file search text or provider file reference was lost")
	}
}
