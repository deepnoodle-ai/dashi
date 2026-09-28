package messages

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

const hostedWebSearchResponse = `{
	"id":"msg_1","type":"message","model":"test-model",
	"stop_reason":"end_turn",
	"content":[
		{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"kernel"}},
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
			{"type":"web_search_result","url":"https://example.com","title":"Result",
			 "encrypted_content":"secret-replay"}
		]},
		{"type":"text","text":"Found it."}
	]
}`

func TestHostedWebSearchReplayPreservesPrivateResult(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := codec{origin: client.client.Origin("test-model")}
	result, err := wire.decode(
		[]byte(hostedWebSearchResponse),
		llm.ModelRef{Provider: "anthropic", Model: "test-model"},
	)
	if err != nil {
		t.Fatalf("decode hosted response: %v", err)
	}
	if len(result.Message.Content) != 3 {
		t.Fatal("hosted blocks or answer were lost")
	}
	if result.Message.Content[0].HostedUse == nil {
		t.Fatal("server call became an executable client tool call")
	}
	resource := result.Message.Content[1].HostedResult.Output[0].Resource
	if resource == nil || resource.URI != "https://example.com" {
		t.Fatal("search result reference was lost")
	}
	request, err := client.Prepare(context.Background(), llm.Input{
		Model: "test-model",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("Find kernel")),
			result.Message,
			llm.UserMessage(llm.Text("Thanks")),
		},
	})
	if err != nil {
		t.Fatalf("prepare hosted history: %v", err)
	}
	var body struct {
		Messages []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 || len(body.Messages[1].Content) != 3 {
		t.Fatal("hosted history did not retain its block order")
	}
	var replay struct {
		Content []struct {
			EncryptedContent string `json:"encrypted_content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body.Messages[1].Content[1], &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Content[0].EncryptedContent != "secret-replay" {
		t.Fatal("private result data was lost during replay")
	}
}

func TestHostedToolDefinitionsPrepareWithoutSchema(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	web, err := NewWebSearchTool(WebSearchToolOptions{
		AllowedDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := NewWebFetchTool("", 2)
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(context.Background(), llm.Input{
		Model:    "test-model",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Search"))},
		Tools:    []tool.Definition{web, fetch, NewCodeExecutionTool("")},
	})
	if err != nil {
		t.Fatalf("prepare hosted definitions: %v", err)
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 3 {
		t.Fatal("hosted definitions were omitted")
	}
	if body.Tools[0]["type"] != WebSearchType {
		t.Fatal("web search wire version changed")
	}
	if body.Tools[1]["max_uses"] != float64(2) {
		t.Fatal("web fetch limit was omitted")
	}
}

func TestMCPToolsetSetsServerAndBetaHeader(t *testing.T) {
	var beta string
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		beta = request.Header.Get("Anthropic-Beta")
		_, _ = io.WriteString(writer, `{
			"id":"msg_1","type":"message","model":"test-model",
			"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]
		}`)
	}))
	defer server.Close()
	client, err := New(Config{
		Provider:         "anthropic",
		APIKey:           "test",
		Endpoint:         server.URL,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := NewMCPToolset("", MCPToolsetOptions{
		Name: "docs",
		URL:  "https://example.com/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(context.Background(), llm.Input{
		Model:    "test-model",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Read"))},
		Tools:    []tool.Definition{definition},
	})
	if err != nil {
		t.Fatalf("prepare MCP toolset: %v", err)
	}
	var body struct {
		MCPServers []map[string]any `json:"mcp_servers"`
		Tools      []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.MCPServers) != 1 || len(body.Tools) != 1 {
		t.Fatal("MCP server and toolset must be paired")
	}
	if body.Tools[0]["mcp_server_name"] != "docs" {
		t.Fatal("MCP toolset lost its server name")
	}
	if _, err := client.Send(context.Background(), llm.Call{Request: request}); err != nil {
		t.Fatalf("send MCP toolset request: %v", err)
	}
	if beta != "mcp-client-2025-11-20" {
		t.Fatal("MCP beta header was omitted")
	}
}

func TestHostedWebSearchStreamMatchesCompleteProjection(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      API,
		Model:    "test-model",
		Scope:    "test-scope",
	}
	wire := codec{origin: origin}
	stream := `data: {"type":"message_start","message":{"id":"msg_1","type":"message","model":"test-model","content":[]}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"kernel\"}"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.com","encrypted_content":"secret-replay"}]}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

data: {"type":"message_stop"}

`
	result, err := wire.readStream(
		io.NopCloser(strings.NewReader(stream)),
		llm.Result{
			Requested: llm.ModelRef{
				Provider: "anthropic",
				Model:    "test-model",
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("decode hosted stream: %v", err)
	}
	if len(result.Message.Content) != 2 ||
		result.Message.Content[0].HostedUse == nil ||
		result.Message.Content[1].HostedResult == nil {
		t.Fatal("hosted stream lost the call or result")
	}
	if string(result.Message.Content[0].HostedUse.Input) != `{"query":"kernel"}` {
		t.Fatal("streamed hosted arguments were lost")
	}
	if !strings.Contains(
		string(result.Message.Content[1].Replay.Raw),
		"secret-replay",
	) {
		t.Fatal("streamed private result data was lost")
	}
}

func TestMCPResultExposesImageAndResource(t *testing.T) {
	wire := codec{origin: content.Origin{
		Provider: "anthropic",
		API:      API,
		Model:    "test-model",
		Scope:    "test-scope",
	}}
	part, err := wire.decodeContent(json.RawMessage(`{
		"type":"mcp_tool_result","tool_use_id":"mcptoolu_1",
		"content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},
			{"type":"resource","resource":{"uri":"https://example.com/report","mimeType":"text/html"}}
		]
	}`))
	if err != nil {
		t.Fatalf("decode MCP result: %v", err)
	}
	if part.HostedResult == nil || len(part.HostedResult.Output) != 2 {
		t.Fatal("MCP image or resource was lost")
	}
	if part.HostedResult.Output[0].Image == nil {
		t.Fatal("MCP image was not projected as image data")
	}
	resource := part.HostedResult.Output[1].Resource
	if resource == nil || resource.URI != "https://example.com/report" {
		t.Fatal("MCP resource link was lost")
	}
}
