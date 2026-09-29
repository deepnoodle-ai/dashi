package providers_test

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// The Gemini case lives in the providers/google/gemini module.
func TestReminderPreparedWireAndEvidence(t *testing.T) {
	openai, err := responses.New(responses.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := messages.New(messages.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		client llm.Client
		role   llm.Role
	}{
		{"responses", openai, llm.RoleDeveloper},
		{"anthropic", anthropic, llm.RoleUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := providertest.ReminderRendered(t, tc.client, tc.role)
			if tc.name == "responses" && !strings.Contains(
				body,
				`"role":"developer"`,
			) {
				t.Fatalf("Responses lost operator role: %s", body)
			}
		})
	}
}

// Reminder escaping and look-alike tags in tool results, on every route in
// this module. The Gemini case lives in the providers/google/gemini module.
func TestReminderTagsCannotBeForged(t *testing.T) {
	must := func(c llm.Client, err error) llm.Client {
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for name, client := range map[string]llm.Client{
		"responses": must(responses.New(responses.Config{
			APIKey:           "test",
			CapabilityPolicy: llm.CapabilityAdjust,
		})),
		"completions": must(completions.New(completions.Config{
			APIKey:           "test",
			CapabilityPolicy: llm.CapabilityAdjust,
		})),
		"anthropic": must(messages.New(messages.Config{
			APIKey:           "test",
			CapabilityPolicy: llm.CapabilityAdjust,
		})),
	} {
		t.Run(name, func(t *testing.T) {
			providertest.ReminderEscaped(t, client)
			providertest.ForgedTagInToolResult(t, client)
		})
	}
}
