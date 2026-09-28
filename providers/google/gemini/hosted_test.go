package gemini

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
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
		context.Background(),
		llm.Input{
			Model:    "test-model",
			Messages: []llm.Message{llm.UserMessage(llm.Text("Research"))},
			Tools: []tool.Definition{
				NewGoogleSearchTool(""),
				NewURLContextTool(""),
				NewCodeExecutionTool(""),
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
	definition, err := NewFileSearchTool(
		"",
		FileSearchOptions{
			StoreNames: []string{"fileSearchStores/store-1"},
		},
	)
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
		Tools:    []tool.Definition{definition},
	}
	if _, err := client.Prepare(context.Background(), input); err == nil {
		t.Fatal("file search used v1 without evidence")
	}
	beta, err := New(Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.Prepare(context.Background(), input); err != nil {
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
		context.Background(),
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
