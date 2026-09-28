package responses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func structuredFixtureModels(endpoint string) map[string]llm.ModelInfo {
	return map[string]llm.ModelInfo{
		"fixture-model": {
			Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{
				llm.FeatureText:             llm.SupportSupported,
				llm.FeatureMaxTokens:        llm.SupportSupported,
				llm.FeatureStreaming:        llm.SupportSupported,
				llm.FeatureStructuredOutput: llm.SupportSupported,
			}},
			Evidence: model.ModelEvidence{
				Model:       "fixture-model",
				API:         API,
				Endpoint:    endpoint,
				Owner:       "Dashi fixtures",
				Source:      "structured_output_test.go",
				Method:      model.EvidenceFixture,
				QualifiedAt: time.Date(2026, time.September, 28, 0, 0, 0, 0, time.UTC),
			},
		},
	}
}

func TestStructuredOutputPrepareOwnsExactSchema(t *testing.T) {
	client, err := New(Config{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	input := structuredInput(false)
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.OutputSchema[0] = '['
	snapshot := request.Snapshot()
	if !slices.Equal(snapshot.Input.OutputSchema, structuredSchema) ||
		!slices.Contains(snapshot.Unverified, llm.FeatureStructuredOutput) {
		t.Fatalf("preparation lost schema or support uncertainty: %+v", snapshot)
	}
	var body struct {
		Text struct {
			Format struct {
				Type   string          `json:"type"`
				Name   string          `json:"name"`
				Strict bool            `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Text.Format.Type != "json_schema" ||
		body.Text.Format.Name != "dashi_output" ||
		!body.Text.Format.Strict ||
		!reflect.DeepEqual(body.Text.Format.Schema, structuredSchema) {
		t.Fatalf("native format missing or rewritten: %s", request.Body())
	}
	restored, err := llm.NewRequest(snapshot)
	if err != nil || !slices.Equal(restored.Body(), request.Body()) {
		t.Fatalf("restored body changed: %v", err)
	}
}

func TestStructuredOutputPrepareRejectsUnsupportedInputs(t *testing.T) {
	client, err := New(Config{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	optional := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":[],"additionalProperties":false}`)
	badRoot := json.RawMessage(`{"type":"array","items":{"type":"string"}}`)
	withTools := structuredInput(false)
	withTools.Tools = []tool.Definition{{
		Name:   "lookup",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindLocal,
	}}
	for _, tc := range []struct {
		name  string
		input llm.Input
		kind  llm.ErrorCategory
		path  string
	}{
		{
			name:  "invalid root",
			input: llm.Input{Model: "m", OutputSchema: badRoot},
			kind:  llm.ErrorInvalidRequest,
			path:  "output schema root",
		},
		{
			name:  "strict subset",
			input: llm.Input{Model: "m", OutputSchema: optional},
			kind:  llm.ErrorUnsupported,
			path:  ".required",
		},
		{
			name:  "current tools",
			input: withTools,
			kind:  llm.ErrorUnsupported,
			path:  "current-call tools",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Prepare(t.Context(), tc.input)
			failure, ok := errors.AsType[*llm.Error](err)
			if !ok || failure.Category != tc.kind || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}
	strict, err := New(Config{
		APIKey:           "fixture-key",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if err != nil {
		t.Fatal(err)
	}
	strictInput := structuredInput(false)
	strictInput.Model = "gpt-4o" // catalog knows text and max_tokens, not this feature
	_, err = strict.Prepare(t.Context(), strictInput)
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorUnsupported ||
		!strings.Contains(err.Error(), string(llm.FeatureStructuredOutput)) {
		t.Fatalf("unknown strict support: %+v %v", failure, err)
	}
}

func TestStructuredOutputCustomEndpointRequiresEvidence(t *testing.T) {
	endpoint := "https://example.test/responses"
	client, err := New(Config{
		Provider: "fixture",
		Endpoint: endpoint,
		APIKey:   "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(t.Context(), structuredInput(false))
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorUnsupported ||
		!strings.Contains(err.Error(), "endpoint-scoped ModelInfo") {
		t.Fatalf("missing evidence: %+v %v", failure, err)
	}
	client, err = New(Config{
		Provider: "fixture",
		Endpoint: endpoint,
		APIKey:   "key",
		Models:   structuredFixtureModels(endpoint),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prepare(t.Context(), structuredInput(false)); err != nil {
		t.Fatalf("matching explicit evidence: %v", err)
	}
}

func TestStructuredOutputRejectsOversizedSchema(t *testing.T) {
	client, err := New(Config{APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	input := structuredInput(false)
	input.OutputSchema = json.RawMessage(`{"type":"object","description":"` +
		strings.Repeat("x", 32<<10) + `","properties":{},"required":[],"additionalProperties":false}`)
	_, err = client.Prepare(t.Context(), input)
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorInvalidRequest ||
		!strings.Contains(err.Error(), "32768 bytes") {
		t.Fatalf("oversized schema: %+v %v", failure, err)
	}
}

func fixtureResponse(status string, output []any) []byte {
	response := map[string]any{
		"id":     "resp_fixture",
		"object": "response",
		"model":  "fixture-model",
		"status": status,
		"output": output,
		"usage": map[string]any{
			"input_tokens":  7,
			"output_tokens": 3,
		},
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if status == "failed" {
		response["error"] = map[string]any{"code": "fixture_failure"}
	}
	raw, _ := json.Marshal(response)
	return raw
}

func fixtureText(parts ...string) []any {
	content := make([]any, 0, len(parts))
	for _, text := range parts {
		content = append(content, map[string]any{
			"type": "output_text",
			"text": text,
		})
	}
	return []any{map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": content,
	}}
}

func TestStructuredOutputTerminalFixtures(t *testing.T) {
	valid := fixtureResponse("completed", fixtureText(`{"label":"ok"}`))
	invalid := fixtureResponse("completed", fixtureText(`{"label":2}`))
	malformed := fixtureResponse("completed", fixtureText(`{"label":`))
	multiple := fixtureResponse("completed", fixtureText(`{"label":`, `"ok"}`))
	refusal := fixtureResponse("completed", []any{map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []any{map[string]any{
			"type":    "refusal",
			"refusal": "cannot answer",
		}},
	}})
	toolUse := fixtureResponse("completed", []any{map[string]any{
		"type":      "function_call",
		"call_id":   "call_1",
		"name":      "unexpected",
		"arguments": `{}`,
	}})
	userText := fixtureResponse("completed", []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "output_text",
			"text": `{"label":"ok"}`,
		}},
	}})
	for _, tc := range []struct {
		name   string
		body   []byte
		output string
		stop   llm.StopKind
		error  llm.ErrorCategory
	}{
		{name: "valid", body: valid, output: `{"label":"ok"}`, stop: llm.StopFinished},
		{name: "schema mismatch", body: invalid, stop: llm.StopIncomplete, error: llm.ErrorProtocol},
		{name: "malformed", body: malformed, stop: llm.StopIncomplete, error: llm.ErrorProtocol},
		{name: "multiple text parts", body: multiple, stop: llm.StopIncomplete, error: llm.ErrorProtocol},
		{name: "refusal", body: refusal, stop: llm.StopRefusal},
		{
			name: "output limit",
			body: fixtureResponse("incomplete", fixtureText(`{"label":`)),
			stop: llm.StopOutputLimit,
		},
		{name: "unexpected tool", body: toolUse, stop: llm.StopToolUse},
		{
			name:  "user-role text",
			body:  userText,
			stop:  llm.StopIncomplete,
			error: llm.ErrorProtocol,
		},
		{
			name:  "failed provider",
			body:  fixtureResponse("failed", nil),
			stop:  llm.StopIncomplete,
			error: llm.ErrorProtocol,
		},
	} {
		for _, stream := range []bool{false, true} {
			mode := "/blocking"
			if stream {
				mode = "/stream"
			}
			t.Run(tc.name+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+string(tc.body)+"}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(tc.body)
				}))
				defer server.Close()
				client, err := New(Config{
					Provider: "test",
					Endpoint: server.URL,
					APIKey:   "key",
					Models:   structuredFixtureModels(server.URL),
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := llm.Generate(t.Context(), client, structuredInput(stream), nil)
				if tc.error == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					failure, ok := errors.AsType[*llm.Error](err)
					if !ok || failure.Category != tc.error {
						t.Fatalf("failure=%+v err=%v", failure, err)
					}
				}
				if result.Stop.Kind != tc.stop || string(result.Output) != tc.output ||
					result.Usage == nil || result.Usage.InputTokens != 7 || result.Raw == nil {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			})
		}
	}
}

func TestStructuredOutputRejectsOversizedAnswer(t *testing.T) {
	big := fixtureResponse("completed", fixtureText(
		`{"label":"`+strings.Repeat("x", 256<<10)+`"}`,
	))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	}))
	defer server.Close()
	client, err := New(Config{
		Provider: "test",
		Endpoint: server.URL,
		APIKey:   "key",
		Models:   structuredFixtureModels(server.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.Generate(t.Context(), client, structuredInput(false), nil)
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorProtocol ||
		result.Output != nil || result.Stop.Kind != llm.StopIncomplete ||
		result.Usage == nil {
		t.Fatalf("oversized answer: result=%+v failure=%+v err=%v", result, failure, err)
	}
}

func TestStructuredOutputInterruptedStreamKeepsPreview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"model\":\"fixture-model\",\"output\":[]}}\n\n"+
				"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n"+
				"data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()
	client, err := New(Config{
		Provider: "test",
		Endpoint: server.URL,
		APIKey:   "key",
		Models:   structuredFixtureModels(server.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.Generate(t.Context(), client, structuredInput(true), nil)
	if err == nil || result.Stop.Kind != llm.StopIncomplete ||
		result.Output != nil || result.Message.Text() != "partial" {
		t.Fatalf("interrupted stream: result=%+v err=%v", result, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestStructuredOutputLateTotalTimeoutKeepsOutputAbsent(t *testing.T) {
	valid := fixtureResponse("completed", fixtureText(`{"label":"ok"}`))
	stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"model\":\"fixture-model\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + string(valid) + "}\n\n"
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    r,
		}, nil
	})}
	total := 30 * time.Millisecond
	client, err := New(Config{
		Provider: "test",
		Endpoint: "https://example.test/responses",
		APIKey:   "key",
		Client:   httpClient,
		Timeouts: llm.HTTPTimeouts{Total: &total},
		Models:   structuredFixtureModels("https://example.test/responses"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(t.Context(), structuredInput(true))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Send(t.Context(), llm.Call{
		Request: request,
		Sink: func(llm.Delta) {
			time.Sleep(3 * total)
		},
	})
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorTransport ||
		failure.Reason != llm.ReasonCallTimeout ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	if result.Output != nil || result.Stop.Kind != llm.StopIncomplete ||
		result.Message.Text() != `{"label":"ok"}` || result.Usage == nil {
		t.Fatalf("late timeout returned validated output: %+v", result)
	}
}
