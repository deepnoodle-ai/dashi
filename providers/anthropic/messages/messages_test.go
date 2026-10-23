package messages

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestPrepareReasoningEffort(t *testing.T) {
	const endpoint = "https://fixture.test/v1/messages"
	for _, tc := range []struct {
		name    string
		listed  []string // the model's catalog reasoning_efforts
		policy  llm.CapabilityPolicy
		effort  string
		want    string
		wantErr string
	}{
		{name: "xhigh listed", listed: []string{"low", "medium", "high", "xhigh", "max"}, policy: llm.CapabilityStrict, effort: "xhigh", want: "xhigh"},
		{name: "xhigh unlisted model levels", policy: llm.CapabilityStrict, effort: "xhigh", want: "xhigh"},
		{name: "xhigh alias", listed: []string{"low", "medium", "high", "xhigh", "max"}, policy: llm.CapabilityAdjust, effort: "extra-high", want: "xhigh"},
		{name: "xhigh not listed adjusts", listed: []string{"low", "medium", "high", "max"}, policy: llm.CapabilityAdjust, effort: "xhigh", want: "high"},
		{name: "xhigh not listed strict", listed: []string{"low", "medium", "high", "max"}, policy: llm.CapabilityStrict, effort: "xhigh", wantErr: `"xhigh" is unsupported`},
		{name: "unknown level", policy: llm.CapabilityStrict, effort: "turbo", wantErr: `"turbo" is unsupported`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := llm.ModelInfo{
				Evidence: llm.ModelEvidence{
					Model:       "fixture",
					API:         API,
					Endpoint:    endpoint,
					Owner:       "test",
					Source:      "keyless effort fixture",
					Method:      llm.EvidenceFixture,
					QualifiedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
				},
				Capabilities: llm.Capabilities{
					Features: map[llm.Feature]llm.Support{
						llm.FeatureText:            llm.SupportSupported,
						llm.FeatureMaxTokens:       llm.SupportSupported,
						llm.FeatureReasoningEffort: llm.SupportSupported,
					},
					ReasoningEfforts: tc.listed,
				},
			}
			client, err := New(Config{
				Provider:         "fixture",
				Endpoint:         endpoint,
				Models:           map[string]llm.ModelInfo{"fixture": info},
				CapabilityPolicy: tc.policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{
				Model:    "fixture",
				Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
				Settings: llm.Settings{
					Reasoning: &llm.ReasoningSettings{Effort: tc.effort},
				},
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				OutputConfig struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
			}
			if err := json.Unmarshal(request.Body(), &body); err != nil {
				t.Fatal(err)
			}
			if body.OutputConfig.Effort != tc.want {
				t.Fatalf("effort = %q, want %q; body %s", body.OutputConfig.Effort, tc.want, request.Body())
			}
		})
	}
}

func TestPrepareRejectsPrefillOnModelsThatRejectIt(t *testing.T) {
	prefill := []llm.Message{
		llm.UserMessage(llm.Text("Name a color.")),
		llm.AssistantMessage(llm.Text("The color is")),
	}
	for _, policy := range []llm.CapabilityPolicy{
		llm.CapabilityAdjust,
		llm.CapabilityStrict,
	} {
		client, err := New(Config{APIKey: "test", CapabilityPolicy: policy})
		if err != nil {
			t.Fatal(err)
		}
		// Haiku 5.5 rejects a prefill even without reasoning.
		_, err = client.Prepare(t.Context(), llm.Input{
			Model:    "claude-haiku-5-5",
			Messages: prefill,
		})
		failure, ok := errors.AsType[*llm.Error](err)
		if !ok || failure.Category != llm.ErrorUnsupported ||
			!strings.Contains(err.Error(), "end the history with a user message") {
			t.Fatalf("policy %q: Haiku 5.5 prefill error = %v", policy, err)
		}
		// Haiku 4.5 accepts one when thinking is off.
		request, err := client.Prepare(t.Context(), llm.Input{
			Model:    "claude-haiku-4-5",
			Messages: prefill,
		})
		if err != nil {
			t.Fatalf("policy %q: Haiku 4.5 prefill was rejected: %v", policy, err)
		}
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(request.Body(), &body); err != nil {
			t.Fatal(err)
		}
		if n := len(body.Messages); n == 0 || body.Messages[n-1].Role != "assistant" {
			t.Fatalf("policy %q: prefill was not sent last: %s", policy, request.Body())
		}
	}
}

// taskBudgetClient is a Messages client for the catalog endpoint, so the
// bundled catalog applies, whose transport records the Anthropic-Beta header
// and body of each request instead of sending it.
func taskBudgetClient(t *testing.T, policy llm.CapabilityPolicy) (*Client, *string, *string) {
	t.Helper()
	var beta, body string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		beta = r.Header.Get("Anthropic-Beta")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"m",` +
				`"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`)),
			Request: r,
		}, nil
	})
	client, err := New(Config{
		Provider:         "anthropic",
		APIKey:           "test",
		Client:           &http.Client{Transport: transport},
		CapabilityPolicy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, &beta, &body
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func taskBudgetInput(model string, budget llm.TaskBudget) llm.Input {
	return llm.Input{
		Model:    model,
		Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))},
		Settings: llm.Settings{
			Reasoning:  &llm.ReasoningSettings{Effort: "medium"},
			TaskBudget: &budget,
		},
	}
}

func TestTaskBudgetSentBesideEffortWithBetaHeader(t *testing.T) {
	client, beta, sent := taskBudgetClient(t, llm.CapabilityStrict)
	remaining := 12_345
	request, err := client.Prepare(t.Context(), taskBudgetInput(
		"claude-haiku-5-5",
		llm.TaskBudget{Total: 40_000, Remaining: &remaining},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(t.Context(), llm.Call{Request: request}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		OutputConfig json.RawMessage `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(*sent), &body); err != nil {
		t.Fatal(err)
	}
	want := `{"effort":"medium","task_budget":{"remaining":12345,"total":40000,"type":"tokens"}}`
	if string(body.OutputConfig) != want {
		t.Fatalf("output_config = %s, want %s", body.OutputConfig, want)
	}
	if *beta != betaTaskBudgets || len(request.Warnings()) != 0 {
		t.Fatalf("beta = %q, warnings = %v", *beta, request.Warnings())
	}
}

// The task budget beta joins the MCP beta in one header.
func TestTaskBudgetBetaMergesWithMCPBeta(t *testing.T) {
	client, beta, _ := taskBudgetClient(t, llm.CapabilityAdjust)
	definition, err := NewMCPTool(MCPToolOptions{Name: "docs", URL: "https://example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	input := taskBudgetInput("claude-opus-5-5", llm.TaskBudget{Total: 40_000})
	input.Tools = []tool.Definition{definition.Definition}
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(t.Context(), llm.Call{Request: request}); err != nil {
		t.Fatal(err)
	}
	if *beta != betaMCPClient+","+betaTaskBudgets {
		t.Fatalf("beta = %q", *beta)
	}
}

// A model documented without task budgets gets none and no beta header,
// with a warning; strict refuses the request.
func TestTaskBudgetOmittedOnUnsupportedModel(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "claude-opus-4-6", "claude-haiku-4-5"} {
		t.Run(model, func(t *testing.T) {
			client, beta, sent := taskBudgetClient(t, llm.CapabilityAdjust)
			input := taskBudgetInput(model, llm.TaskBudget{Total: 40_000})
			input.Settings.Reasoning = nil
			request, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			warnings := request.Warnings()
			if len(warnings) != 1 || warnings[0].Code != llm.WarningSettingOmitted ||
				warnings[0].Field != llm.SettingTaskBudget {
				t.Fatalf("warnings = %+v", warnings)
			}
			if _, err := client.Send(t.Context(), llm.Call{Request: request}); err != nil {
				t.Fatal(err)
			}
			if *beta != "" || strings.Contains(*sent, "task_budget") {
				t.Fatalf("beta = %q, body = %s", *beta, *sent)
			}
			strict, _, _ := taskBudgetClient(t, llm.CapabilityStrict)
			_, err = strict.Prepare(t.Context(), input)
			if e, ok := errors.AsType[*llm.Error](err); !ok || e.Category != llm.ErrorUnsupported {
				t.Fatalf("strict err = %v, want unsupported", err)
			}
		})
	}
}

// A Total below the API minimum is raised to it with a warning under the
// adjust policy, keeping Remaining; strict refuses it.
func TestTaskBudgetBelowMinimum(t *testing.T) {
	remaining := 5_000
	input := taskBudgetInput("claude-haiku-5-5", llm.TaskBudget{Total: 10_000, Remaining: &remaining})
	client, _, _ := taskBudgetClient(t, llm.CapabilityAdjust)
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	warnings := request.Warnings()
	if len(warnings) != 1 || warnings[0].Code != llm.WarningSettingClamped ||
		warnings[0].Field != llm.SettingTaskBudget {
		t.Fatalf("warnings = %+v", warnings)
	}
	if !strings.Contains(string(request.Body()), `"task_budget":{"remaining":5000,"total":20000,"type":"tokens"}`) {
		t.Fatalf("body = %s", request.Body())
	}
	if input.Settings.TaskBudget.Total != 10_000 {
		t.Fatal("preparation changed the caller's budget")
	}
	strict, _, _ := taskBudgetClient(t, llm.CapabilityStrict)
	if _, err := strict.Prepare(t.Context(), input); err == nil ||
		!strings.Contains(err.Error(), "below the minimum 20000") {
		t.Fatalf("strict err = %v", err)
	}
}

// Support for a model outside the catalog is unknown: the adjust policy sends
// the budget and records it as unverified, and strict refuses it.
func TestTaskBudgetUnknownModel(t *testing.T) {
	input := taskBudgetInput("claude-custom", llm.TaskBudget{Total: 30_000})
	input.Settings.Reasoning = nil
	client, _, _ := taskBudgetClient(t, llm.CapabilityAdjust)
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request.Body()), `"task_budget":{"total":30000,"type":"tokens"}`) ||
		!slices.Contains(request.Snapshot().Unverified, llm.FeatureTaskBudget) {
		t.Fatalf("body = %s, unverified = %v", request.Body(), request.Snapshot().Unverified)
	}
	strict, _, _ := taskBudgetClient(t, llm.CapabilityStrict)
	if _, err := strict.Prepare(t.Context(), input); err == nil {
		t.Fatal("strict accepted a task budget of unknown support")
	}
}
