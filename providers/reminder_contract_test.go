package providers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

func TestReminderPreparedWireAndEvidence(t *testing.T) {
	openai, err := responses.New(responses.Config{APIKey: "test", UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := messages.New(messages.Config{APIKey: "test", UnknownCapabilities: llm.UnknownAllow})
	if err != nil {
		t.Fatal(err)
	}
	google, err := gemini.New(gemini.Config{APIKey: "test", UnknownCapabilities: llm.UnknownAllow})
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
		{"gemini", google, llm.RoleUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := llm.Input{Model: "example-model", Messages: []llm.Message{
				llm.UserMessage(llm.Text("question")),
				llm.ReminderMessage(llm.Reminder{Name: "scope", Tier: llm.ReminderOperator, Content: "visible reminder", Details: json.RawMessage(`{"private":"hidden detail"}`)}),
			}}
			req, err := tc.client.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := req.Snapshot()
			if len(snapshot.ContentChanges) != 1 || snapshot.ContentChanges[0].Action != llm.ContentChangeRenderReminder || snapshot.ContentChanges[0].ToRole != tc.role || snapshot.Input.Messages[1].Content[0].Kind != llm.ContentReminder {
				t.Fatalf("snapshot=%+v", snapshot)
			}
			body := string(req.Body())
			if !strings.Contains(body, "visible reminder") || strings.Contains(body, "hidden detail") {
				t.Fatalf("wire privacy failure: %s", body)
			}
			if tc.name == "responses" && !strings.Contains(body, `"role":"developer"`) {
				t.Fatalf("Responses lost operator role: %s", body)
			}
		})
	}
}
