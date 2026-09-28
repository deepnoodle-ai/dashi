//go:build live

package providers_test

import (
	"os"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// Run only with explicit credentials and selected model IDs:
// DASHI_LIVE_TEST=1 OPENAI_MODEL=... ANTHROPIC_MODEL=...
// go test -tags live ./providers -run TestLiveAgentFinalFacts -v
// The full run makes four paid requests. A subtest filter selects one provider
// and needs only that provider's key and model ID. The Gemini equivalent lives
// in the providers/google/gemini module.
func TestLiveAgentFinalFacts(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		providertest.LiveFinalFacts(
			t,
			os.Getenv("ANTHROPIC_API_KEY"),
			os.Getenv("ANTHROPIC_MODEL"),
			func(key string) (llm.Client, error) {
				return messages.New(messages.Config{
					APIKey:           key,
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		)
	})
	t.Run("openai", func(t *testing.T) {
		providertest.LiveFinalFacts(
			t,
			os.Getenv("OPENAI_API_KEY"),
			os.Getenv("OPENAI_MODEL"),
			func(key string) (llm.Client, error) {
				return responses.New(responses.Config{
					APIKey:           key,
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		)
	})
}
