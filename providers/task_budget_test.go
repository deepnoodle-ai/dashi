package providers_test

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/ollama"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// APIs without task budgets omit the setting with a warning, and strict
// preparation refuses it.
func TestOtherAPIsOmitTaskBudgetWithWarning(t *testing.T) {
	clients := map[string]func(llm.CapabilityPolicy) llm.Client{
		"responses": func(policy llm.CapabilityPolicy) llm.Client {
			return newResponses(t, responses.Config{
				Provider:         "openai",
				CapabilityPolicy: policy,
				APIKey:           "test",
				Endpoint:         "http://unused.invalid",
			})
		},
		"completions": func(policy llm.CapabilityPolicy) llm.Client {
			p, err := completions.New(completions.Config{
				Provider:         "openai",
				CapabilityPolicy: policy,
				APIKey:           "test",
				Endpoint:         "http://unused.invalid",
			})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		"ollama": func(policy llm.CapabilityPolicy) llm.Client {
			p, err := ollama.New(ollama.Config{CapabilityPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
	}
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			r := modelInput()
			r.Settings.TaskBudget = &llm.TaskBudget{Total: 40_000}
			prepared, err := newClient(llm.CapabilityAdjust).Prepare(t.Context(), r)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			warnings := prepared.Warnings()
			if len(warnings) != 1 ||
				warnings[0].Code != llm.WarningSettingOmitted ||
				warnings[0].Field != llm.SettingTaskBudget {
				t.Fatalf("warnings = %+v", warnings)
			}
			if prepared.Effective().TaskBudget != nil {
				t.Fatal("effective settings kept the task budget")
			}
			_, err = newClient(llm.CapabilityStrict).Prepare(t.Context(), r)
			if e, ok := errors.AsType[*llm.Error](err); !ok ||
				e.Category != llm.ErrorUnsupported {
				t.Fatalf("strict err = %v, want unsupported", err)
			}
		})
	}
}
