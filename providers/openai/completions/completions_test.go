package completions

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

const completeResponse = `{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","content":"Hello","tool_calls":[{"id":"call-1","type":"function","function":{"name":"search","arguments":"{\"q\":\"yes\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":2}}}`

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	p, err := New(Config{
		Provider:         "openai",
		Endpoint:         endpoint,
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testInput(stream bool) llm.Input {
	return llm.Input{
		Model:    "requested",
		Stream:   stream,
		System:   "Be concise.",
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		Tools: []tool.Definition{
			{Name: "search", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		Settings: llm.Settings{
			ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		},
	}
}

func TestPrepareSendsFrozenBodyOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("missing configured authorization header")
		}
		got, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(got, &body); err != nil {
			t.Errorf("invalid dispatched JSON: %v", err)
		}
		if body["model"] != "requested" || body["stream"] != false {
			t.Errorf("wrong dispatched request: %s", got)
		}
		io.WriteString(w, completeResponse)
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 0 || !strings.Contains(
		string(prepared.Body()),
		`"parameters":{"type":"object"}`,
	) {
		t.Fatalf("preparation dispatched or lost schema: %s", prepared.Body())
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 ||
		result.ID != "chat-1" ||
		result.Served.Model != "served" ||
		result.Stop.Kind != llm.StopToolUse || result.Message.Text() != "Hello" ||
		len(result.Message.Content) != 2 ||
		result.Message.Content[1].ToolCall.ID != "call-1" {
		t.Fatalf("lost Chat Completions result: %+v", result)
	}
	if result.Usage == nil ||
		result.Usage.InputTokens != 7 ||
		result.Usage.CacheReadInputTokens != 5 ||
		result.Usage.OutputTokens != 4 || result.Usage.ReasoningTokens != 2 {
		t.Fatalf("lost disjoint usage: %+v", result.Usage)
	}
}

func TestStreamAccumulatesFinalUsageAndToolCall(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(
			string(body),
			`"stream_options":{"include_usage":true}`,
		) {
			t.Errorf("stream usage was not requested: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"id":"chat-1","model":"served","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"yes\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":2}}}`,
		} {
			io.WriteString(w, "data: "+event+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(true))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []llm.Delta
	result, err := p.Send(
		t.Context(),
		llm.Call{
			Request: prepared,
			Sink: func(d llm.Delta) {
				deltas = append(deltas, d)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 ||
		result.Message.Text() != "Hello" ||
		result.Stop.Kind != llm.StopToolUse ||
		len(result.Message.Content) != 2 ||
		result.Message.Content[1].ToolCall.ID != "call-1" ||
		result.Usage == nil || result.Usage.InputTokens != 7 || len(deltas) < 4 {
		t.Fatalf(
			"stream lost final content or usage: result=%+v deltas=%+v",
			result,
			deltas,
		)
	}
}

func TestReportedEstimateStaysWithResultAcrossTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "complete"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(
							w,
							"data: "+`{"model":"served","service_tier":"priority","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n",
						)
						io.WriteString(
							w,
							"data: "+`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"estimated_cost":0.25}}`+"\n\n",
						)
						io.WriteString(w, "data: [DONE]\n\n")
						return
					}
					io.WriteString(
						w,
						`{"model":"served","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"estimated_cost":0.25}}`,
					)
				},
			))
			defer server.Close()
			client, err := New(Config{
				Provider:          "deepinfra",
				Endpoint:          server.URL,
				APIKey:            "test",
				CapabilityPolicy:  llm.CapabilityAdjust,
				ReportedCostField: "estimated_cost",
				CostCurrency:      "USD",
			})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := client.Prepare(
				t.Context(),
				llm.Input{
					Model:    "requested",
					Stream:   stream,
					Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			var deltas []llm.Delta
			result, err := client.Send(
				t.Context(),
				llm.Call{
					Request: prepared,
					Sink: func(delta llm.Delta) {
						deltas = append(deltas, delta)
					},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil || result.Usage.InputTokens != 10 ||
				result.Cost == nil ||
				result.Cost.Total != money.MustParseAmount("0.25") ||
				result.Cost.Source != llm.CostSourceProviderEstimate ||
				result.ServiceTier != "priority" {
				t.Fatalf("final accounting was incomplete: %+v", result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var restored llm.Result
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.Cost == nil ||
				restored.Cost.Total != money.MustParseAmount("0.25") ||
				restored.ServiceTier != "priority" ||
				strings.Contains(string(encoded), `"usage":{"cost"`) {
				t.Fatal("cost or serving tier was lost in the stored result")
			}
			if stream {
				foundUsage := false
				for _, delta := range deltas {
					if delta.Kind == llm.DeltaUsage {
						foundUsage = delta.Usage != nil &&
							delta.Usage.InputTokens == 10
					}
				}
				if !foundUsage {
					t.Fatal("stream lost provisional token usage")
				}
			}
		})
	}
}

func TestReportedZeroCostDiffersFromMissingCost(t *testing.T) {
	wire := codec{costField: "cost", costCurrency: "USD"}
	var zero money.Amount
	known := wire.reportedCost(Usage{Cost: &zero})
	if known == nil || known.Total != 0 ||
		known.Source != llm.CostSourceProviderReported {
		t.Fatal("provider-reported zero cost was treated as unknown")
	}
	if unknown := wire.reportedCost(Usage{}); unknown != nil {
		t.Fatal("missing provider cost was treated as known zero")
	}
}

func TestPrepareRejectsUnsupportedContentBeforeDispatch(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	input := testInput(false)
	input.Messages = []llm.Message{
		llm.UserMessage(llm.Block(content.Block{
			Kind: content.KindAudio,
			Audio: &content.Audio{
				Source: content.Source{Data: []byte("audio"), MIMEType: "audio/wav"},
			},
		})),
	}
	_, err := p.Prepare(t.Context(), input)
	failure, ok := errors.AsType[*llm.Error](err)
	if !ok || failure.Category != llm.ErrorUnsupported || attempts.Load() != 0 {
		t.Fatalf("unsupported audio dispatched or was misclassified: %v", err)
	}
}

func TestStructuredReasoningRetainsSameOriginReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(
			w,
			`{"id":"r","model":"requested","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_details":[{"type":"reasoning.text","text":"private"}]},"finish_reason":"stop"}]}`,
		)
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(false))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Message.Content) != 2 ||
		result.Message.Content[0].Opaque == nil {
		t.Fatalf("reasoning details were lost: %+v", result.Message)
	}
	input := testInput(false)
	input.Messages = append(input.Messages, result.Message)
	_, err = p.Prepare(t.Context(), input)
	if err != nil {
		t.Fatalf("same-origin reasoning replay failed: %v", err)
	}
}

func TestInterruptedStreamKeepsPartialToolArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(
			w,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`+"\n\n",
		)
	}))
	defer server.Close()
	p := testClient(t, server.URL)
	prepared, err := p.Prepare(t.Context(), testInput(true))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Send(t.Context(), llm.Call{Request: prepared})
	if err == nil ||
		result.Stop.Kind != llm.StopIncomplete ||
		len(result.Message.Content) != 1 ||
		result.Message.Content[0].Opaque == nil ||
		!result.Message.Content[0].Opaque.Partial ||
		!strings.Contains(string(result.Message.Content[0].Opaque.Raw), `{\"q\":`) {
		t.Fatalf("unfinished tool call was lost: result=%+v err=%v", result, err)
	}
}

func TestPrepareStatesToolErrorInText(t *testing.T) {
	p := testClient(t, "https://unused.invalid")
	input := testInput(false)
	input.Messages = append(
		input.Messages,
		llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &tool.Call{ID: "call-1", Name: "search", Args: json.RawMessage(`{}`)},
		}),
		llm.Message{
			Role: llm.RoleUser,
			Content: []llm.Content{{
				Kind: llm.ContentToolResult,
				ToolResult: &llm.ToolResult{
					CallID:  "call-1",
					Output:  tool.TextOutput("The call was denied."),
					IsError: true,
				},
			}},
		},
	)
	prepared, err := p.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(prepared.Body(), &body); err != nil {
		t.Fatal(err)
	}
	last := body.Messages[len(body.Messages)-1]
	if last["role"] != "tool" || last["tool_call_id"] != "call-1" ||
		last["content"] != "Tool failed:\nThe call was denied." {
		t.Fatalf("tool error encoded as %v", last)
	}
	changes := prepared.ContentChanges()
	if len(changes) != 1 ||
		changes[0].Action != llm.ContentChangeEncodeToolError ||
		changes[0].MessageIndex != 2 ||
		len(prepared.Warnings()) != 0 {
		t.Fatalf("changes=%+v warnings=%+v", changes, prepared.Warnings())
	}
}

func TestPrepareNamesOutputLimitForEachDestination(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		endpoint string
		model    string
		field    string
	}{
		{
			name:     "OpenAI non-reasoning model",
			endpoint: openAIEndpoint,
			model:    "gpt-4.1",
			field:    "max_completion_tokens",
		},
		{
			name:     "OpenAI reasoning model",
			endpoint: openAIEndpoint,
			model:    "o3",
			field:    "max_completion_tokens",
		},
		{
			name:     "proxy serving a reasoning model",
			provider: "proxy",
			endpoint: "https://proxy.example/v1/chat/completions",
			model:    "gpt-5",
			field:    "max_completion_tokens",
		},
		{
			name:     "OpenRouter OpenAI model",
			provider: "openrouter",
			endpoint: "https://openrouter.ai/api/v1/chat/completions",
			model:    "openai/gpt-4o",
			field:    "max_tokens",
		},
		{
			name:     "Mistral model starting with o",
			provider: "mistral",
			endpoint: "https://api.mistral.ai/v1/chat/completions",
			model:    "open-mistral-nemo",
			field:    "max_tokens",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := New(Config{
				Provider: test.provider,
				Endpoint: test.endpoint,
				APIKey:   "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := p.Prepare(
				t.Context(),
				llm.Input{
					Model:    test.model,
					Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(request.Body(), &body); err != nil {
				t.Fatal(err)
			}
			want := float64(request.Effective().MaxTokens)
			if want == 0 || body[test.field] != want {
				t.Fatalf(
					"body = %s, want %s = %v",
					request.Body(),
					test.field,
					want,
				)
			}
			_, legacy := body["max_tokens"]
			_, current := body["max_completion_tokens"]
			if legacy && current {
				t.Fatal("both output limit fields were sent")
			}
		})
	}
}
