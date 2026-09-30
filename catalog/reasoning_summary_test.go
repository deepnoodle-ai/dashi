package catalog

import "github.com/deepnoodle-ai/dashi/llm"

// These exact IDs are documented by the public summary guides checked on
// 2026-09-30. No sibling or reasoning-effort inference qualifies another ID.
func summaryClaims() []claimSource {
	return []claimSource{
		{url: "https://developers.openai.com/api/docs/guides/reasoning#reasoning-summaries", models: []string{"gpt-6-astra", "o4-mini"}, feature: llm.FeatureReasoningSummary, support: llm.SupportSupported},
		{url: "https://ai.google.dev/gemini-api/docs/generate-content/thinking#thought-summaries", models: []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "gemini-3.1-flash-lite", "gemini-3-flash-preview", "gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"}, feature: llm.FeatureReasoningSummary, support: llm.SupportSupported},
		{url: "https://platform.claude.com/docs/en/build-with-claude/thinking#controlling-thinking-display", models: claudeAll, feature: llm.FeatureReasoningSummary, support: llm.SupportSupported},
	}
}
