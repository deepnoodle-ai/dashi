package catalog

import (
	"slices"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Prompt cache facts, read 2026-09-29. See design/prompt-cache.md.
const (
	anthropicPromptCaching = "https://platform.claude.com/docs/en/" +
		"build-with-claude/prompt-caching"
	openAIPromptCaching = "https://developers.openai.com/api/docs/guides/" +
		"prompt-caching"
)

var (
	// openAIExtendedRetention is OpenAI's list of models that accept
	// prompt_cache_retention "24h", copied rather than derived from versions.
	openAIExtendedRetention = []string{
		"gpt-5.5",
		"gpt-5.5-pro",
		"gpt-5.4",
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5",
		"gpt-4.1",
	}
	// openAIThirtyMinuteCache lists the models on OpenAI's caching from
	// GPT-5.6: one 30-minute lifetime, writes at 1.25x, and a key that
	// separates accounting.
	openAIThirtyMinuteCache = []string{
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.6-cyber",
		"gpt-daybreak-blue-latest",
		"gpt-daybreak-red-latest",
	}
)

// promptCacheClaims states that every Claude model and every OpenAI
// Responses model accepts a prompt cache request.
func promptCacheClaims() []claimSource {
	var responses []string
	for _, e := range Entries("openai") {
		if slices.Contains(e.APIs, "openai.responses") {
			responses = append(responses, e.Model)
		}
	}
	return []claimSource{
		{
			url:     anthropicPromptCaching,
			models:  claudeAll,
			feature: llm.FeaturePromptCache,
			support: llm.SupportSupported,
		},
		{
			url:     openAIPromptCaching,
			models:  responses,
			feature: llm.FeaturePromptCache,
			support: llm.SupportSupported,
		},
	}
}

func TestPromptCacheLifetimesFollowTheSourceLists(t *testing.T) {
	const minute = time.Minute
	for _, provider := range Providers() {
		for _, e := range Entries(provider) {
			var ttls []time.Duration
			keyDefault := false
			if provider == "openai" &&
				slices.Contains(e.APIs, "openai.responses") {
				switch {
				case slices.Contains(openAIThirtyMinuteCache, e.Model):
					ttls = []time.Duration{30 * minute}
				case slices.Contains(openAIExtendedRetention, e.Model):
					ttls = []time.Duration{5 * minute, 24 * time.Hour}
					keyDefault = true
				default:
					ttls = []time.Duration{5 * minute}
					keyDefault = true
				}
			}
			c := e.Capabilities
			if !slices.Equal(c.CacheTTLs, ttls) ||
				c.CacheKeyDefault != keyDefault {
				t.Fatalf(
					"%s %s cache ttls %v key default %v, want %v %v",
					provider,
					e.Model,
					c.CacheTTLs,
					c.CacheKeyDefault,
					ttls,
					keyDefault,
				)
			}
		}
	}
}

func TestAutoCacheMarksOnlyTheClaudeAPI(t *testing.T) {
	for _, provider := range Providers() {
		for _, d := range Destinations(provider) {
			want := d.API == "anthropic.messages" &&
				d.Endpoint == "https://api.anthropic.com/v1/messages"
			if d.AutoCache != want || AutoCache(d.API, d.Endpoint) != want {
				t.Fatalf("%s %s auto cache = %v, want %v",
					provider, d.Endpoint, d.AutoCache, want)
			}
		}
	}
	if AutoCache("anthropic.messages", "https://proxy.example/v1/messages") {
		t.Fatal("an undocumented endpoint caches by default")
	}
}
