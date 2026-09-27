//go:build live

package providers_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
)

// Run only with explicit credentials and selected model IDs:
// KERNEL_LIVE_TEST=1 OPENAI_MODEL=... ANTHROPIC_MODEL=... GEMINI_MODEL=...
// go test -tags live ./providers -run TestLiveAgentFinalFacts -v
// This makes six paid requests and qualifies only these selected routes.
func TestLiveAgentFinalFacts(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_TEST=1 to make paid provider calls")
	}
	type fixture struct {
		name, key, model string
		new              func(string) (llm.Client, error)
	}
	cases := []fixture{
		{"anthropic", os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_MODEL"), func(key string) (llm.Client, error) {
			return messages.New(messages.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
		}},
		{"openai", os.Getenv("OPENAI_API_KEY"), os.Getenv("OPENAI_MODEL"), func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
		}},
		{"gemini", os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_MODEL"), func(key string) (llm.Client, error) {
			return gemini.New(gemini.Config{APIKey: key, UnknownCapabilities: llm.UnknownAllow})
		}},
	}
	for _, tc := range cases {
		if tc.key == "" || tc.model == "" {
			t.Fatalf("%s needs its API key and MODEL environment variable", tc.name)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.new(tc.key)
			if err != nil {
				t.Fatal(err)
			}
			for _, stream := range []bool{false, true} {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				input := llm.Input{Model: tc.model, Stream: stream, Settings: llm.Settings{MaxTokens: 32}, Messages: []llm.Message{llm.UserMessage(llm.Text("Reply with one short greeting."))}}
				request, err := client.Prepare(ctx, input)
				if err == nil {
					var result llm.Result
					result, err = client.Send(ctx, request, nil)
					if err == nil && (result.Stop.Kind != llm.StopFinished || result.Message.Text() == "" || result.Requested.Model != tc.model) {
						t.Errorf("stream=%t incomplete final facts: %+v", stream, result)
					}
				}
				cancel()
				if err != nil {
					t.Errorf("stream=%t: %v", stream, err)
				}
			}
		})
	}
}
