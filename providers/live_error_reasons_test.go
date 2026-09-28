//go:build live

package providers_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/kernel/internal/providertest"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/deepinfra"
	"github.com/deepnoodle-ai/kernel/providers/grok"
	"github.com/deepnoodle-ai/kernel/providers/mistral"
	"github.com/deepnoodle-ai/kernel/providers/openai/completions"
	"github.com/deepnoodle-ai/kernel/providers/openai/responses"
	"github.com/deepnoodle-ai/kernel/providers/openrouter"
)

// TestLiveErrorReasons provokes each classified rejection against real
// providers and logs the provider's error body as recorded evidence.
//
// KERNEL_LIVE_TEST=1 go test -tags live ./providers -run TestLiveErrorReasons -v
//
// Each provider reads its usual API key variable and is skipped without one.
// Filter a subtest such as '^TestLiveErrorReasons/anthropic$' to test one
// provider. Media probes send a few bytes. Context probes send a text input
// about 1.5 times the model's context window; providers reject these before
// inference, so they are not billed, but an input that did fit would cost
// roughly its token count at the model's input price (at most about US$0.30
// per probe for the models below). Grok has no context probe: current xAI
// models have windows too large to exceed cheaply. Anthropic no longer
// rejects input that fits only without max_tokens (claude-haiku-4-5 accepted
// 168k input with max_tokens 64000), so only the oversized prompt is probed.
// The Gemini probe lives in the providers/google/gemini module.
func TestLiveErrorReasons(t *testing.T) {
	probes := map[string]providertest.ErrorProbe{
		"anthropic": {
			KeyEnv: "ANTHROPIC_API_KEY",
			Model:  "claude-haiku-4-5-20251001",
			Window: 200_000,
			Media:  true,
			New: func() (llm.Client, error) {
				return messages.New(messages.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		"openai-responses": {
			KeyEnv: "OPENAI_API_KEY",
			Model:  "gpt-4o-mini",
			Window: 128_000,
			Media:  true,
			New: func() (llm.Client, error) {
				return responses.New(responses.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		"openai-completions": {
			KeyEnv: "OPENAI_API_KEY",
			Model:  "gpt-4o-mini",
			Window: 128_000,
			Media:  true,
			New: func() (llm.Client, error) {
				return completions.New(completions.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		"mistral": {
			KeyEnv: "MISTRAL_API_KEY",
			Model:  "ministral-3b-2512",
			Window: 131_072,
			New: func() (llm.Client, error) {
				return mistral.New(mistral.Config{CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		"grok": {
			KeyEnv: "XAI_API_KEY",
			Model:  "grok-4.20-0309-non-reasoning",
			Media:  true,
			New: func() (llm.Client, error) {
				return grok.New(grok.Config{CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		"openrouter": {
			KeyEnv: "OPENROUTER_API_KEY",
			Model:  "mistralai/mistral-small-24b-instruct-2501",
			Window: 32_768,
			New: func() (llm.Client, error) {
				return openrouter.New(openrouter.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		"deepinfra": {
			KeyEnv: "DEEP_INFRA_API_KEY",
			Model:  "meta-llama/Meta-Llama-3.1-8B-Instruct",
			Window: 131_072,
			New: func() (llm.Client, error) {
				return deepinfra.New(deepinfra.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
	}
	for _, name := range slices.Sorted(maps.Keys(probes)) {
		t.Run(name, func(t *testing.T) {
			providertest.LiveErrorReasons(t, probes[name])
		})
	}
}
