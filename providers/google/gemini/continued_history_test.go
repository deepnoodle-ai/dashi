package gemini

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

// A turn continued after max_tokens stores two assistant messages back to
// back. Gemini prepares a later request from that history and sends both
// replies in order.
func TestContinuedHistoryPrepares(t *testing.T) {
	client, err := New(Config{APIKey: "test", CapabilityPolicy: llm.CapabilityAdjust})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.Prepare(t.Context(), llm.Input{
		Model: "gemini-test",
		Messages: []llm.Message{
			llm.UserMessage(llm.Text("hi")),
			llm.AssistantMessage(llm.Text("partial")),
			llm.AssistantMessage(llm.Text("continued")),
			llm.UserMessage(llm.Text("next")),
		},
		Settings: llm.Settings{MaxTokens: 4096},
	})
	if err != nil {
		t.Fatalf("Prepare rejected the history: %v", err)
	}
	body := string(req.Body())
	i, j := strings.Index(body, "partial"), strings.Index(body, "continued")
	if i < 0 || j < i || !strings.Contains(body[j:], "next") {
		t.Fatalf("body lost the replies' order: %s", body)
	}
}
