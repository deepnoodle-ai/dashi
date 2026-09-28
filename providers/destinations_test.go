package providers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/deepinfra"
	"github.com/deepnoodle-ai/kernel/providers/grok"
	"github.com/deepnoodle-ai/kernel/providers/meta"
	"github.com/deepnoodle-ai/kernel/providers/mistral"
	"github.com/deepnoodle-ai/kernel/providers/ollama"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/providers/openrouter"
)

func TestDiveDestinationAdapters(t *testing.T) {
	tests := []struct {
		name, api, response string
		new                 func(string) (llm.Client, error)
	}{
		{"deepinfra", completions.API,
			`{"model":"served","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"estimated_cost":0.25}}`,
			func(url string) (llm.Client, error) {
				return deepinfra.New(deepinfra.Config{Endpoint: url, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
			}},
		{"ollama", messages.API,
			`{"type":"message","id":"m","model":"served","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`,
			func(url string) (llm.Client, error) {
				return ollama.New(ollama.Config{Endpoint: url, CapabilityPolicy: llm.CapabilityAdjust})
			}},
		{"mistral", completions.API,
			`{"model":"served","choices":[{"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"consider"}]},{"type":"text","text":"ok"}]},"finish_reason":"stop"}]}`,
			func(url string) (llm.Client, error) {
				return mistral.New(mistral.Config{Endpoint: url, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
			}},
		{"openrouter", completions.API,
			`{"model":"served","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"cost":0.5}}`,
			func(url string) (llm.Client, error) {
				return openrouter.New(openrouter.Config{Endpoint: url, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
			}},
		{"grok", responses.API,
			`{"object":"response","id":"r","model":"served","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
			func(url string) (llm.Client, error) {
				return grok.New(grok.Config{Endpoint: url, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
			}},
		{"meta", responses.API,
			`{"object":"response","id":"r","model":"served","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
			func(url string) (llm.Client, error) {
				return meta.New(meta.Config{Endpoint: url, APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent, _ = io.ReadAll(r.Body)
				if tt.name == "openrouter" && (r.Header.Get("HTTP-Referer") != "https://deepnoodle.ai" ||
					r.Header.Get("X-Title") != "Deep Noodle") {
					t.Errorf("OpenRouter attribution headers were lost: %+v", r.Header)
				}
				io.WriteString(w, tt.response)
			}))
			defer server.Close()
			client, err := tt.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if client.Provider() != tt.name || client.API() != tt.api {
				t.Fatalf("wrong destination identity: %s %s", client.Provider(), client.API())
			}
			prepared, err := client.Prepare(context.Background(), llm.Input{Model: "requested",
				Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}})
			if err != nil {
				t.Fatal(err)
			}
			if len(sent) != 0 {
				t.Fatal("Prepare dispatched")
			}
			result, err := client.Send(context.Background(), prepared, nil)
			if err != nil || result.Message.Text() != "ok" || result.Requested.Provider != tt.name ||
				result.Served.Provider != tt.name || result.Stop.Kind != llm.StopFinished {
				t.Fatalf("destination lost response: result=%+v err=%v", result, err)
			}
			if !json.Valid(sent) || string(sent) != string(prepared.Body()) {
				t.Fatal("destination changed prepared body")
			}
			if tt.name == "deepinfra" && (result.Usage == nil || result.Cost == nil ||
				result.Cost.Total != 0.25 || result.Cost.Source != llm.CostSourceProviderEstimate) {
				t.Fatalf("DeepInfra estimate provenance lost: %+v", result)
			}
			if tt.name == "openrouter" && (result.Usage == nil || result.Cost == nil ||
				result.Cost.Total != 0.5 || result.Cost.Source != llm.CostSourceProviderReported) {
				t.Fatalf("OpenRouter reported cost provenance lost: %+v", result)
			}
			if tt.name == "mistral" && (len(result.Message.Content) != 2 ||
				result.Message.Content[0].Reasoning == nil || result.Message.Content[0].Reasoning.Text != "consider") {
				t.Fatalf("Mistral thinking part was lost: %+v", result.Message)
			}
		})
	}
}

func TestDeepInfraRejectsBinaryDocument(t *testing.T) {
	client, err := deepinfra.New(deepinfra.Config{Endpoint: "http://localhost:1", APIKey: "test",
		CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	input := llm.Input{Model: "requested", Messages: []llm.Message{llm.UserMessage(llm.Block(
		content.DocumentBlock(content.Source{Data: []byte("pdf"), MIMEType: "application/pdf"})))}}
	if _, err := client.Prepare(context.Background(), input); err == nil {
		t.Fatal("DeepInfra accepted a binary document")
	}
}
