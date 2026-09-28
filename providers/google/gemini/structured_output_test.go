package gemini

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestNativeStructuredOutputIsNotYetQualified(t *testing.T) {
	client, err := New(Config{APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(t.Context(), llm.Input{
		Model:        "fixture-model",
		Messages:     []llm.Message{llm.UserMessage(llm.Text("Classify"))},
		OutputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	})
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Category != llm.ErrorUnsupported {
		t.Fatalf("Prepare error = %v", err)
	}
}
