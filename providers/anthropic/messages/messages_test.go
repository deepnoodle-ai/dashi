package messages

import (
	"encoding/json"
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
