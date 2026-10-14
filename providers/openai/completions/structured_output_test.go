package completions

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/model"
	"github.com/deepnoodle-ai/dashi/tool"
)

var structuredSchema = json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`)

func structuredInput(stream bool) llm.Input {
	return llm.Input{
		Model:        "fixture-model",
		Messages:     []llm.Message{llm.UserMessage(llm.Text("Classify"))},
		OutputSchema: slices.Clone(structuredSchema),
		Stream:       stream,
	}
}

func structuredClient(t *testing.T, endpoint string, support llm.Support) *Client {
	t.Helper()
	client, err := New(Config{
		Provider: "fixture",
		Endpoint: endpoint,
		APIKey:   "key",
		Models: map[string]llm.ModelInfo{
			"fixture-model": {
				Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{
					llm.FeatureText:             llm.SupportSupported,
					llm.FeatureStreaming:        llm.SupportSupported,
					llm.FeatureStructuredOutput: support,
				}},
				Evidence: model.ModelEvidence{
					Model:       "fixture-model",
					API:         API,
					Endpoint:    endpoint,
					Owner:       "Dashi fixtures",
					Source:      "structured_output_test.go",
					Method:      model.EvidenceFixture,
					QualifiedAt: time.Date(2026, time.October, 13, 0, 0, 0, 0, time.UTC),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestStructuredOutputPrepareSendsResponseFormat(t *testing.T) {
	client, err := New(Config{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(t.Context(), structuredInput(false))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string          `json:"name"`
				Strict bool            `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	format := body.ResponseFormat
	if format.Type != "json_schema" || format.JSONSchema.Name != "dashi_output" ||
		!format.JSONSchema.Strict ||
		string(format.JSONSchema.Schema) != string(structuredSchema) {
		t.Fatalf("response_format missing or rewritten: %s", request.Body())
	}
}

func TestStructuredOutputPrepareRejectsUnsupportedInputs(t *testing.T) {
	client, err := New(Config{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	optional := structuredInput(false)
	optional.OutputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":[],"additionalProperties":false}`)
	withTools := structuredInput(false)
	withTools.Tools = []tool.Definition{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
	for _, tc := range []struct {
		name  string
		input llm.Input
		want  string
	}{
		{"strict subset", optional, ".required"},
		{"current tools", withTools, "current-call tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Prepare(t.Context(), tc.input)
			failure, ok := errors.AsType[*llm.Error](err)
			if !ok || failure.Category != llm.ErrorUnsupported || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}
}

func TestStructuredOutputCustomEndpointRequiresEvidence(t *testing.T) {
	endpoint := "https://example.test/v1/chat/completions"
	for _, tc := range []struct {
		name    string
		client  *Client
		allowed bool
	}{
		{"no evidence", testClient(t, endpoint), false},
		{"unsupported", structuredClient(t, endpoint, llm.SupportUnsupported), false},
		{"supported", structuredClient(t, endpoint, llm.SupportSupported), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.Prepare(t.Context(), structuredInput(false))
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := errors.AsType[*llm.Error](err)
			if !ok || failure.Category != llm.ErrorUnsupported {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}
}

func TestStructuredOutputValidatesFinishedReply(t *testing.T) {
	reply := func(content string) string {
		raw, _ := json.Marshal(content)
		return `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":` +
			string(raw) + `},"finish_reason":"stop"}]}`
	}
	streamed := func(content string) string {
		raw, _ := json.Marshal(content)
		return dialectEvents(
			`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":`+string(raw)+`}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		)
	}
	refused := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","refusal":"no"},"finish_reason":"stop"}]}`
	for _, tc := range []struct {
		name     string
		response string
		stream   bool
		output   string
		failure  string
	}{
		{"valid", reply(`{"label":"spam"}`), false, `{"label":"spam"}`, ""},
		{"valid stream", streamed(` {"label":"spam"}`), true, `{"label":"spam"}`, ""},
		{"invalid", reply(`{"label":3}`), false, "", "output_schema"},
		{"not JSON", reply(`spam`), false, "", "output_schema"},
		{"refusal", refused, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.response)
			}))
			defer server.Close()
			client := structuredClient(t, server.URL, llm.SupportSupported)
			request, err := client.Prepare(t.Context(), structuredInput(tc.stream))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Send(t.Context(), llm.Call{Request: request})
			if tc.failure != "" {
				failure, ok := errors.AsType[*llm.Error](err)
				if !ok || failure.Category != llm.ErrorProtocol ||
					!strings.Contains(err.Error(), tc.failure) || result.Output != nil {
					t.Fatalf("failure=%+v err=%v output=%s", failure, err, result.Output)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Output) != tc.output {
				t.Fatalf("output = %s, want %s", result.Output, tc.output)
			}
		})
	}
}
