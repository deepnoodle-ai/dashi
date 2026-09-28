package gemini

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPrepareKeepsThinkingBudgetBelowOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		maxTokens int
		budget    int
		want      int
	}{
		{
			name:   "budget fits the default",
			budget: 2048,
			want:   2048,
		},
		{
			name:   "budget at the default",
			budget: llm.DefaultMaxTokens,
			want:   llm.DefaultMaxTokens - 1,
		},
		{
			name:      "budget above an explicit limit",
			maxTokens: 4096,
			budget:    8192,
			want:      4095,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(Config{
				Provider: "google",
				APIKey:   "test",
				Endpoint: "https://example.test/v1beta/models/{model}:generateContent",
			})
			if err != nil {
				t.Fatal(err)
			}
			input := sampleInput(false)
			input.Settings.MaxTokens = test.maxTokens
			input.Settings.Reasoning = &llm.ReasoningSettings{
				BudgetTokens: test.budget,
			}
			request, err := client.Prepare(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				GenerationConfig struct {
					MaxOutputTokens int `json:"maxOutputTokens"`
					ThinkingConfig  struct {
						ThinkingBudget int `json:"thinkingBudget"`
					} `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			if err := json.Unmarshal(request.Body(), &body); err != nil {
				t.Fatal(err)
			}
			config := body.GenerationConfig
			if config.ThinkingConfig.ThinkingBudget != test.want {
				t.Fatalf(
					"thinking budget = %d, want %d",
					config.ThinkingConfig.ThinkingBudget,
					test.want,
				)
			}
			if config.ThinkingConfig.ThinkingBudget >= config.MaxOutputTokens {
				t.Fatal("thinking budget leaves no room for an answer")
			}
			clamped := test.want != test.budget
			warnings := request.Warnings()
			if clamped != (len(warnings) == 1) {
				t.Fatalf("budget warnings = %+v", warnings)
			}
			if clamped && (warnings[0].Code != llm.WarningSettingClamped ||
				warnings[0].Field != llm.SettingReasoning) {
				t.Fatalf("budget clamp recorded as %+v", warnings[0])
			}
		})
	}
}
