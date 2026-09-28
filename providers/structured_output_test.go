package providers_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/grok"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
)

func TestNativeStructuredOutputRejectsUnqualifiedRoutes(t *testing.T) {
	constructors := []struct {
		name string
		new  func() (llm.Client, error)
	}{
		{"Anthropic Messages", func() (llm.Client, error) {
			return messages.New(messages.Config{APIKey: "fixture"})
		}},
		{"OpenAI Chat Completions", func() (llm.Client, error) {
			return completions.New(completions.Config{APIKey: "fixture"})
		}},
		{"xAI Responses", func() (llm.Client, error) {
			return grok.New(grok.Config{APIKey: "fixture"})
		}},
		{"Meta Responses", func() (llm.Client, error) {
			return meta.New(meta.Config{APIKey: "fixture"})
		}},
	}
	input := llm.Input{
		Model:        "fixture-model",
		Messages:     []llm.Message{llm.UserMessage(llm.Text("Classify"))},
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`),
	}
	for _, tc := range constructors {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.new()
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Prepare(t.Context(), input)
			var failure *llm.Error
			if !errors.As(err, &failure) || failure.Category != llm.ErrorUnsupported {
				t.Fatalf("Prepare error = %v", err)
			}
		})
	}
}
