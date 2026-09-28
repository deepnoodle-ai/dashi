//go:build live

package providers_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/providers/anthropic/messages"
	"github.com/deepnoodle-ai/kernel/providers/deepinfra"
	"github.com/deepnoodle-ai/kernel/providers/google/gemini"
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
func TestLiveErrorReasons(t *testing.T) {
	if os.Getenv("KERNEL_LIVE_TEST") != "1" {
		t.Skip("set KERNEL_LIVE_TEST=1 to make provider calls")
	}
	type probe struct {
		name, keyEnv, model string
		new                 func() (llm.Client, error)
		// window is the model's context window in tokens; zero skips the
		// context probe.
		window int
		// media sends a corrupt PNG and an unsupported image type.
		media bool
	}
	probes := []probe{
		{
			name:   "anthropic",
			keyEnv: "ANTHROPIC_API_KEY",
			model:  "claude-haiku-4-5-20251001",
			window: 200_000,
			media:  true,
			new: func() (llm.Client, error) {
				return messages.New(messages.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		{
			name:   "openai-responses",
			keyEnv: "OPENAI_API_KEY",
			model:  "gpt-4o-mini",
			window: 128_000,
			media:  true,
			new: func() (llm.Client, error) {
				return responses.New(responses.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		{
			name:   "openai-completions",
			keyEnv: "OPENAI_API_KEY",
			model:  "gpt-4o-mini",
			window: 128_000,
			media:  true,
			new: func() (llm.Client, error) {
				return completions.New(completions.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		{
			name:   "gemini",
			keyEnv: "GEMINI_API_KEY",
			model:  "gemini-2.5-flash-lite",
			window: 1_048_576,
			media:  true,
			new: func() (llm.Client, error) {
				return gemini.New(gemini.Config{CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		{
			name:   "mistral",
			keyEnv: "MISTRAL_API_KEY",
			model:  "ministral-3b-2512",
			window: 131_072,
			new: func() (llm.Client, error) {
				return mistral.New(mistral.Config{CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		{
			name:   "grok",
			keyEnv: "XAI_API_KEY",
			model:  "grok-4.20-0309-non-reasoning",
			media:  true,
			new: func() (llm.Client, error) {
				return grok.New(grok.Config{CapabilityPolicy: llm.CapabilityAdjust})
			},
		},
		{
			name:   "openrouter",
			keyEnv: "OPENROUTER_API_KEY",
			model:  "mistralai/mistral-small-24b-instruct-2501",
			window: 32_768,
			new: func() (llm.Client, error) {
				return openrouter.New(openrouter.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
		{
			name:   "deepinfra",
			keyEnv: "DEEP_INFRA_API_KEY",
			model:  "meta-llama/Meta-Llama-3.1-8B-Instruct",
			window: 131_072,
			new: func() (llm.Client, error) {
				return deepinfra.New(deepinfra.Config{
					CapabilityPolicy: llm.CapabilityAdjust,
				})
			},
		},
	}
	// A PNG signature followed by bytes that are not a PNG.
	corrupt := append(
		[]byte("\x89PNG\r\n\x1a\n"),
		[]byte("this is not an image")...,
	)
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			if os.Getenv(p.keyEnv) == "" {
				t.Skipf("%s is not set", p.keyEnv)
			}
			client, err := p.new()
			if err != nil {
				t.Fatal(err)
			}
			send := func(t *testing.T, want llm.ErrorReason, parts ...llm.Content) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				input := llm.Input{
					Model:    p.model,
					Settings: llm.Settings{MaxTokens: 16},
					Messages: []llm.Message{{Role: llm.RoleUser, Content: parts}},
				}
				request, err := client.Prepare(ctx, input)
				if err != nil {
					t.Fatalf("prepare: %v", err)
				}
				result, err := client.Send(ctx, llm.Call{Request: request})
				if err == nil {
					t.Fatalf(
						"request was accepted: served=%s usage=%+v",
						result.Served.Model,
						result.Usage,
					)
				}
				var e *llm.Error
				if !errors.As(err, &e) {
					t.Fatalf("err = %v, want a provider rejection", err)
				}
				t.Logf(
					"status=%d category=%s code=%q reason=%q body=%s",
					e.Status,
					e.Category,
					e.Code,
					e.Reason,
					e.Body,
				)
				if e.Reason != want {
					t.Errorf("reason = %q, want %q", e.Reason, want)
				}
			}
			if p.media {
				t.Run(
					"corrupt image",
					func(t *testing.T) {
						send(
							t,
							llm.ReasonMediaRejected,
							llm.Text("Describe this image."),
							llm.Block(content.ImageBlock(content.Source{
								Data:     corrupt,
								MIMEType: "image/png",
							})),
						)
					},
				)
				t.Run(
					"unsupported image type",
					func(t *testing.T) {
						send(
							t,
							llm.ReasonMediaRejected,
							llm.Text("Describe this image."),
							llm.Block(content.ImageBlock(content.Source{
								Data:     []byte("II*\x00not really a tiff"),
								MIMEType: "image/tiff",
							})),
						)
					},
				)
			}
			if p.window > 0 {
				t.Run(
					"context window",
					func(t *testing.T) {
						// " word" is one token in the tokenizers of all probed
						// models, so this is about 1.5 windows of input.
						send(
							t,
							llm.ReasonContextWindowExceeded,
							llm.Text(strings.Repeat(" word", p.window*3/2)),
						)
					},
				)
			}
		})
	}
}
