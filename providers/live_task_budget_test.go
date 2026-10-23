//go:build live

package providers_test

import (
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
)

// Task budget qualification (design/task-budget.md). Run with:
// DASHI_LIVE_TEST=1 ANTHROPIC_API_KEY=... go test -tags live ./providers \
// -run TestLiveTaskBudget -v
// One small paid call to Haiku 5.5 with the minimum task budget, under the
// strict policy, so the catalog claim, wire format, and beta header must all
// be accepted.
func TestLiveTaskBudget(t *testing.T) {
	if os.Getenv("DASHI_LIVE_TEST") != "1" {
		t.Skip("set DASHI_LIVE_TEST=1 to make paid provider calls")
	}
	client := liveClient(t, "ANTHROPIC", func(key string) (llm.Client, error) {
		return messages.New(messages.Config{APIKey: key, CapabilityPolicy: llm.CapabilityStrict})
	})
	result, err := llm.Generate(t.Context(), client, llm.Input{
		Model:    "claude-haiku-5-5",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Reply with the word ok."))},
		Settings: llm.Settings{
			MaxTokens:  1024,
			Reasoning:  &llm.ReasoningSettings{Effort: "low"},
			TaskBudget: &llm.TaskBudget{Total: 20_000},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stop.Kind != llm.StopFinished || !strings.Contains(strings.ToLower(result.Message.Text()), "ok") {
		t.Fatalf("stop = %+v, reply = %q", result.Stop, result.Message.Text())
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %+v", result.Warnings)
	}
	t.Logf("usage = %+v", result.Usage)
}
