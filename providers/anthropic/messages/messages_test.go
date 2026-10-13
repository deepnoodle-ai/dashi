package messages

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
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
