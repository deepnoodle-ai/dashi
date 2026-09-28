package providers_test

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/internal/providertest"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
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
