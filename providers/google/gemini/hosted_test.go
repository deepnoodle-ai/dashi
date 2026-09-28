package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestHostedBuiltinsPrepareThroughSDK(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(
		t.Context(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Research"))},
			Tools: []tool.Definition{
				NewGoogleSearchTool(GoogleSearchToolOptions{}).Definition,
				NewURLContextTool(URLContextToolOptions{}).Definition,
				NewCodeExecutionTool(CodeExecutionToolOptions{}).Definition,
			},
		},
	)
	if err != nil {
		t.Fatalf("prepare Gemini builtins: %v", err)
	}
	var body struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 3 {
		t.Fatal("SDK dropped built-in tools")
	}
	if body.Tools[0]["googleSearch"] == nil ||
		body.Tools[1]["urlContext"] == nil ||
		body.Tools[2]["codeExecution"] == nil {
		t.Fatal("built-in tool wire names changed")
	}
}

func TestFileSearchRequiresV1Beta(t *testing.T) {
	definition, err := NewFileSearchTool(FileSearchToolOptions{
		StoreNames: []string{"fileSearchStores/store-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{
		Provider:         "google",
		APIKey:           "test",
		Endpoint:         strings.Replace(DefaultEndpoint, "/v1beta/", "/v1/", 1),
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{
		Model:    "test-model",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Find"))},
		Tools:    []tool.Definition{definition.Definition},
	}
	if _, err := client.Prepare(t.Context(), input); err == nil {
		t.Fatal("file search used v1 without evidence")
	}
	beta, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.Prepare(t.Context(), input); err != nil {
		t.Fatalf("prepare v1beta file search: %v", err)
	}
}

func TestCodeExecutionPartsRemainHosted(t *testing.T) {
	client, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	wireClient, err := client.httpClient("test-model")
	if err != nil {
		t.Fatal(err)
	}
	wire := codec{origin: wireClient.Origin("test-model")}
	raw := []byte(`{
		"responseId":"resp_1","modelVersion":"test-model",
		"candidates":[{"finishReason":"STOP","content":{"parts":[
			{"executableCode":{"language":"PYTHON","code":"print(2)"}},
			{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"2"}},
			{"text":"The answer is 2."}
		]}}]
	}`)
	result, err := wire.decode(
		raw,
		llm.ModelRef{
			Provider: "google",
			Model:    "test-model",
		},
	)
	if err != nil {
		t.Fatalf("decode code execution: %v", err)
	}
	if result.Message.Content[0].HostedUse == nil ||
		result.Message.Content[1].HostedResult == nil {
		t.Fatal("provider code became an executable client tool call")
	}
	if result.Message.Content[0].HostedUse.ID !=
		result.Message.Content[1].HostedResult.CallID {
		t.Fatal("code result lost its call pairing")
	}
	_, err = client.Prepare(
		t.Context(),
		llm.Input{
			Model: "test-model",
			Messages: []llm.Message{
				llm.UserMessage(llm.Text("Calculate")),
				result.Message,
				llm.UserMessage(llm.Text("Thanks")),
			},
		},
	)
	if err != nil {
		t.Fatalf("replay code execution: %v", err)
	}
}

func TestHostedToolBindingAcrossGoogleBackends(t *testing.T) {
	vertex, err := New(Config{
		Vertex:           true,
		Project:          "p",
		Location:         "us-central1",
		CredentialScope:  "service-account",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := NewFileSearchTool(FileSearchToolOptions{
		StoreNames: []string{"fileSearchStores/store-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(client *Client, d tool.Definition) error {
		_, err := client.Prepare(t.Context(), llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Research"))},
			Tools:    []tool.Definition{d},
		})
		return err
	}
	const vx = "google-vertex"
	tests := []struct {
		name   string
		client *Client
		tool   tool.Tool
		ok     bool
	}{
		{
			name:   "zero value search on Vertex",
			client: vertex,
			tool:   NewGoogleSearchTool(GoogleSearchToolOptions{}),
			ok:     true,
		},
		{
			name:   "zero value URL context on Vertex",
			client: vertex,
			tool:   NewURLContextTool(URLContextToolOptions{}),
			ok:     true,
		},
		{
			name:   "zero value code execution on Vertex",
			client: vertex,
			tool:   NewCodeExecutionTool(CodeExecutionToolOptions{}),
			ok:     true,
		},
		{
			name:   "explicit Vertex search on Vertex",
			client: vertex,
			tool:   NewGoogleSearchTool(GoogleSearchToolOptions{Provider: vx}),
			ok:     true,
		},
		{
			name:   "Vertex search on Gemini API",
			client: api,
			tool:   NewGoogleSearchTool(GoogleSearchToolOptions{Provider: vx}),
		},
		{
			name:   "other destination on Vertex",
			client: vertex,
			tool: NewGoogleSearchTool(
				GoogleSearchToolOptions{Provider: "proxy"},
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := prepare(tc.client, tc.tool.Definition)
			if (err == nil) != tc.ok ||
				(err != nil && !strings.Contains(err.Error(), "own destination")) {
				t.Fatalf("err=%v; want ok=%t", err, tc.ok)
			}
		})
	}
	// Vertex refuses file search earlier, for its endpoint, so check the
	// binding rule directly: file search stores never cross destinations.
	if boundTo(files.Definition.Hosted, "google-vertex") {
		t.Fatal("file search bound to google crossed to google-vertex")
	}
	if strings.Contains(string(files.Definition.Hosted.Options), "Provider") {
		t.Fatal("file search options recorded the provider binding")
	}
}
