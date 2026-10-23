package catalog

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// claimSource is one provider documentation page that states a behavioral
// claim for the listed models. Every feature claim and effort ladder in the
// catalog must appear here, so an unsourced claim cannot be added without
// naming the page that states it. Text output needs no row: every catalog
// entry is a text model by construction.
type claimSource struct {
	url     string
	models  []string
	feature llm.Feature
	support llm.Support
	// efforts is the documented ladder when feature is reasoning_effort.
	efforts []string
}

// outputLimitSource is one provider documentation page that states the
// maximum output tokens for the listed models. Every max_output_tokens in the
// catalog must appear here, because Fit lowers the default output allowance
// to it and clamps larger requests: a wrong value truncates responses or
// sends a limit the endpoint rejects. A model whose provider documents no
// limit has no row and no value.
type outputLimitSource struct {
	url    string
	models []string
	limit  int
}

// contextWindowSource is one provider documentation page that states the
// context window for the listed models. Every context_window in the catalog
// must appear here, because callers size input against it: a value larger
// than the provider's window invites requests the endpoint rejects. A model
// whose provider documents no window has no row and no value.
type contextWindowSource struct {
	url    string
	models []string
	window int
}

const (
	anthropicOverview = "https://platform.claude.com/docs/en/about-claude/" +
		"models/overview"
	anthropicEffort = "https://platform.claude.com/docs/en/build-with-claude/" +
		"effort"
	anthropicThinking = "https://platform.claude.com/docs/en/" +
		"build-with-claude/thinking-troubleshooting"
	anthropicOpus55Migration = "https://platform.claude.com/docs/en/models/" +
		"opus-5-5/migration-guide"
	anthropicFable5Migration = "https://platform.claude.com/docs/en/models/" +
		"fable-5/migration-guide"
	anthropicFable51Migration = "https://platform.claude.com/docs/en/models/" +
		"fable-5-1/migration-guide"
	anthropicHaiku55Migration = "https://platform.claude.com/docs/en/models/" +
		"haiku-5-5/migration-guide"
	anthropicSonnet55Migration = "https://platform.claude.com/docs/en/models/" +
		"sonnet-5-5/migration-guide"
	anthropicToolUse = "https://platform.claude.com/docs/en/agents-and-tools/" +
		"tool-use/overview"
	anthropicSonnet45 = "https://platform.claude.com/docs/en/models/" +
		"sonnet-4-5/overview"
	anthropicHaiku45 = "https://platform.claude.com/docs/en/models/" +
		"haiku-4-5/overview"
	anthropicPDF = "https://platform.claude.com/docs/en/build-with-claude/" +
		"pdf-support"
	anthropicCitations = "https://platform.claude.com/docs/en/" +
		"build-with-claude/citations"
	anthropicTaskBudgets = "https://platform.claude.com/docs/en/" +
		"build-with-claude/task-budgets"
)

var (
	claudeAll = []string{
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-5-5",
		"claude-opus-5",
		"claude-sonnet-5-5",
		"claude-sonnet-5",
		"claude-haiku-5-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-opus-4-5",
		"claude-opus-4-5-20251101",
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
	}
	claudeAdaptiveOnly = []string{
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-5-5",
		"claude-opus-5",
		"claude-sonnet-5-5",
		"claude-sonnet-5",
		"claude-haiku-5-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
	}
	claudeBudget = []string{
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-opus-4-5",
		"claude-opus-4-5-20251101",
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
	}
)

var claimSources = []claimSource{
	{
		// "All current models support text and image input ... vision,
		// and tool use."
		url:     anthropicOverview,
		models:  claudeAll,
		feature: llm.FeatureImage,
		support: llm.SupportSupported,
	},
	{
		url:     anthropicOverview,
		models:  claudeAll,
		feature: llm.FeatureTools,
		support: llm.SupportSupported,
	},
	{
		// "All active models support PDF processing."
		url:     anthropicPDF,
		models:  claudeAll,
		feature: llm.FeatureDocument,
		support: llm.SupportSupported,
	},
	{
		// "All active models support citations."
		url:     anthropicCitations,
		models:  claudeAll,
		feature: llm.FeatureCitations,
		support: llm.SupportSupported,
	},
	{
		// The supported-models list. The 4.5 and 4.6 generations answer
		// "does not support user-configurable task budgets"; Sonnet 5 is
		// documented as unsupported although it answers 200.
		url: anthropicTaskBudgets,
		models: []string{
			"claude-fable-5-1",
			"claude-mythos-5-1",
			"claude-fable-5",
			"claude-mythos-5",
			"claude-opus-5-5",
			"claude-opus-5",
			"claude-sonnet-5-5",
			"claude-haiku-5-5",
			"claude-opus-4-8",
			"claude-opus-4-7",
		},
		feature: llm.FeatureTaskBudget,
		support: llm.SupportSupported,
	},
	{
		url:     anthropicTaskBudgets,
		models:  append([]string{"claude-sonnet-5"}, claudeBudget...),
		feature: llm.FeatureTaskBudget,
		support: llm.SupportUnsupported,
	},
	{
		// The max and xhigh rows name these models.
		url:     anthropicEffort,
		models:  claudeAdaptiveOnly,
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		// Named in the max row but not the xhigh row.
		url:     anthropicEffort,
		models:  []string{"claude-opus-4-6", "claude-sonnet-4-6"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"low", "medium", "high", "max"},
	},
	{
		// Supported, but named in neither the max nor the xhigh row.
		url: anthropicEffort,
		models: []string{
			"claude-opus-4-5",
			"claude-opus-4-5-20251101",
		},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"low", "medium", "high"},
	},
	{
		// "Default effort: Not supported".
		url: anthropicSonnet45,
		models: []string{
			"claude-sonnet-4-5",
			"claude-sonnet-4-5-20250929",
		},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportUnsupported,
	},
	{
		url: anthropicHaiku45,
		models: []string{
			"claude-haiku-4-5",
			"claude-haiku-4-5-20251001",
		},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportUnsupported,
	},
	{
		// The per-model table rejects "enabled" on these models.
		url:     anthropicThinking,
		models:  claudeAdaptiveOnly,
		feature: llm.FeatureReasoningBudget,
		support: llm.SupportUnsupported,
	},
	{
		// Extended thinking (budget_tokens) is accepted on these models.
		url:     anthropicThinking,
		models:  claudeBudget,
		feature: llm.FeatureReasoningBudget,
		support: llm.SupportSupported,
	},
	{
		// "Setting temperature, top_p, or top_k to any non-default value on
		// Claude Opus 4.7 and later models, including Claude Opus 5.5,
		// returns a 400 error." Sonnet 5 is covered in the same guide.
		url: anthropicOpus55Migration,
		models: []string{
			"claude-opus-5-5",
			"claude-opus-5",
			"claude-opus-4-8",
			"claude-opus-4-7",
			"claude-sonnet-5",
		},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// "Setting temperature, top_p, or top_k to a non-default value
		// returns a 400 error."
		url:     anthropicModelPage("claude-sonnet-5-5"),
		models:  []string{"claude-sonnet-5-5"},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// "Omit temperature, top_p, and top_k, since a non-default value for
		// any of them returns a 400 error."
		url:     anthropicModelPage("claude-haiku-5-5"),
		models:  []string{"claude-haiku-5-5"},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// "The prefill and sampling-parameter restrictions ... carry over
		// from Claude Opus 5 unchanged."
		url:     anthropicFable5Migration,
		models:  []string{"claude-fable-5", "claude-mythos-5"},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// "The prefill restriction, the sampling-parameter restriction ...
		// also carry over"; the two models diverge only in availability,
		// safety classifiers, and Priority Tier.
		url:     anthropicFable51Migration,
		models:  []string{"claude-fable-5-1", "claude-mythos-5-1"},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// "Claude Haiku 5.5 rejects it with a 400 error, even with thinking
		// turned off. End messages with a user turn".
		url:     anthropicHaiku55Migration,
		models:  []string{"claude-haiku-5-5"},
		feature: llm.FeatureAssistantPrefill,
		support: llm.SupportUnsupported,
	},
	{
		// "Prefilling assistant messages returns a 400 error on Claude Opus
		// 4.6 and later Opus models, including Claude Opus 5.5".
		url: anthropicOpus55Migration,
		models: []string{
			"claude-opus-5-5",
			"claude-opus-5",
			"claude-opus-4-8",
			"claude-opus-4-7",
			"claude-opus-4-6",
		},
		feature: llm.FeatureAssistantPrefill,
		support: llm.SupportUnsupported,
	},
	{
		// "Claude Sonnet 5.5 rejects a prefilled last assistant turn with a
		// 400 error, as Claude Sonnet 4.6 and Claude Sonnet 5 do."
		url: anthropicSonnet55Migration,
		models: []string{
			"claude-sonnet-5-5",
			"claude-sonnet-5",
			"claude-sonnet-4-6",
		},
		feature: llm.FeatureAssistantPrefill,
		support: llm.SupportUnsupported,
	},
	{
		// "The prefill and sampling-parameter restrictions ... carry over
		// from Claude Opus 5 unchanged."
		url:     anthropicFable5Migration,
		models:  []string{"claude-fable-5", "claude-mythos-5"},
		feature: llm.FeatureAssistantPrefill,
		support: llm.SupportUnsupported,
	},
	{
		// "Prefill: Prefilling the assistant message returns a 400 error,
		// unchanged from Claude Fable 5."
		url:     anthropicFable51Migration,
		models:  []string{"claude-fable-5-1", "claude-mythos-5-1"},
		feature: llm.FeatureAssistantPrefill,
		support: llm.SupportUnsupported,
	},
	{
		// The pricing table lists tool_choice any and tool for these
		// models; forced choice is rejected on Opus 5.5, Sonnet 5.5, and the
		// 5.1 models.
		url: anthropicToolUse,
		models: []string{
			"claude-opus-5",
			"claude-sonnet-5",
			"claude-haiku-5-5",
			"claude-opus-4-8",
			"claude-opus-4-7",
			"claude-opus-4-6",
			"claude-sonnet-4-6",
			"claude-opus-4-5",
			"claude-opus-4-5-20251101",
			"claude-sonnet-4-5",
			"claude-sonnet-4-5-20250929",
			"claude-haiku-4-5",
			"claude-haiku-4-5-20251001",
		},
		feature: llm.FeatureToolChoice,
		support: llm.SupportSupported,
	},
	{
		// "Reasoning efforts (supported): low, medium, high, xhigh".
		url:     "https://docs.x.ai/developers/models/grok-4.7",
		models:  []string{"grok-4.7"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"low", "medium", "high", "xhigh"},
	},
	{
		url:     "https://docs.x.ai/developers/models/grok-4.6",
		models:  []string{"grok-4.6"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"low", "medium", "high", "xhigh"},
	},
	{
		url:     "https://docs.x.ai/developers/models/grok-4.3",
		models:  []string{"grok-4.3"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"none", "low", "medium", "high", "xhigh"},
	},
	{
		// "none": "Not supported by Muse Spark: returns HTTP 400"; "max":
		// "Standard-tier muse-spark-1.3 only".
		url:     "https://dev.meta.ai/docs/reasoning",
		models:  []string{"muse-spark-1.3"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{
			"minimal",
			"low",
			"medium",
			"high",
			"xhigh",
			"max",
		},
	},
	{
		url: "https://dev.meta.ai/docs/reasoning",
		models: []string{
			"muse-spark-1.3-contributor",
			"muse-spark-1.2",
			"muse-spark-1.2-contributor",
			"muse-spark-1.1",
		},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{"minimal", "low", "medium", "high", "xhigh"},
	},
	{
		// "max": "Supported by muse-wonder-1.0 and muse-spark-1.3"; "none"
		// is "Not supported by Muse Wonder or Muse Spark".
		url:     "https://dev.meta.ai/docs/reasoning",
		models:  []string{"muse-wonder-1.0"},
		feature: llm.FeatureReasoningEffort,
		support: llm.SupportSupported,
		efforts: []string{
			"minimal",
			"low",
			"medium",
			"high",
			"xhigh",
			"max",
		},
	},
	{
		// "Muse Wonder takes text, image, video, audio, and PDF input".
		// Meta's launch post says audio needs confirmation on the release
		// surface, so audio is not claimed.
		url:     "https://dev.meta.ai/docs/models",
		models:  []string{"muse-wonder-1.0"},
		feature: llm.FeatureImage,
		support: llm.SupportSupported,
	},
	{
		url:     "https://dev.meta.ai/docs/models",
		models:  []string{"muse-wonder-1.0"},
		feature: llm.FeatureDocument,
		support: llm.SupportSupported,
	},
	{
		// "Muse Wonder also handles chat completion, image understanding,
		// video and audio understanding, tool calling".
		url:     "https://dev.meta.ai/docs/models",
		models:  []string{"muse-wonder-1.0"},
		feature: llm.FeatureTools,
		support: llm.SupportSupported,
	},
	{
		// "Remove temperature, top_p, and top_k from your requests. If
		// provided, they are ignored." Fit omits it with a warning rather
		// than send a value the model ignores.
		url:     "https://ai.google.dev/gemini-api/docs/latest-model",
		models:  []string{"gemini-4-argon"},
		feature: llm.FeatureTemperature,
		support: llm.SupportUnsupported,
	},
	{
		// The thinkingBudget table gives a range for 2.5 Pro, 2.5 Flash, and
		// 2.5 Flash Lite, and "Gemini 2.5 series models don't support
		// thinkingLevel; use thinkingBudget". Gemini 3 accepts a budget only
		// "for backwards compatibility", so it stays unknown there.
		url:     "https://ai.google.dev/gemini-api/docs/generate-content/thinking",
		models:  gemini25,
		feature: llm.FeatureReasoningBudget,
		support: llm.SupportSupported,
	},
	{
		// "Muse Spark is tuned to run at the default of 1.0 and performs
		// best there".
		url: "https://dev.meta.ai/docs/protocols/chat-completions",
		models: []string{
			"muse-spark-1.3",
			"muse-spark-1.3-contributor",
			"muse-spark-1.2",
			"muse-spark-1.2-contributor",
			"muse-spark-1.1",
		},
		feature: llm.FeatureTemperature,
		support: llm.SupportSupported,
	},
}

// openAIEfforts lists the ladder each OpenAI model page states in its
// "reasoning.effort supports" line. The page URL is derived from the ID.
var openAIEfforts = map[string][]string{
	"gpt-6-astra":   {"low", "medium", "high", "xhigh", "max"},
	"gpt-6.1-sol":   {"low", "medium", "high", "xhigh", "max"},
	"gpt-6-sol":     {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-6-luna":    {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.6":       {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.6-sol":   {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.6-terra": {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.6-luna":  {"none", "low", "medium", "high", "xhigh", "max"},
	"gpt-5.5":       {"none", "low", "medium", "high", "xhigh"},
	"gpt-5.5-pro":   {"medium", "high", "xhigh"},
	"gpt-5.4":       {"none", "low", "medium", "high", "xhigh"},
	"gpt-5.4-pro":   {"medium", "high", "xhigh"},
	"gpt-5.4-mini":  {"none", "low", "medium", "high", "xhigh"},
	"gpt-5.4-nano":  {"none", "low", "medium", "high", "xhigh"},
	"gpt-5.2":       {"none", "low", "medium", "high", "xhigh"},
	"gpt-5.2-pro":   {"medium", "high", "xhigh"},
	"gpt-5.1":       {"none", "low", "medium", "high"},
	"gpt-5":         {"minimal", "low", "medium", "high"},
	"gpt-5-pro":     {"high"},
	"gpt-5.3-codex": {"low", "medium", "high", "xhigh"},
}

// openAINonReasoning names models whose page says they have no reasoning
// step ("non-reasoning model", "without a reasoning step").
var openAINonReasoning = []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano"}

// openAIFeatures lists, for each feature, the models whose page states it:
// image where "Input modalities" names image, tools where "Supported
// features" names function_calling, and structured_output where it names
// structured_outputs. o3-mini takes text input only, the pro models from
// gpt-5.2-pro to gpt-5.4-pro list no structured_outputs, and
// gpt-5-codex-mini has no model page; their support stays unknown. Pages
// name file_uploads without saying which file types, so document stays
// unknown. Checked 2026-10-13.
var openAIFeatures = map[llm.Feature][]string{
	llm.FeatureImage: openAIModelsExcept("o3-mini", "gpt-5-codex-mini"),
	llm.FeatureTools: openAIModelsExcept("gpt-5-codex-mini"),
	llm.FeatureStructuredOutput: openAIModelsExcept(
		"gpt-5.2-pro",
		"gpt-5.4-pro",
		"gpt-5-codex-mini",
	),
}

// googleFeatures lists, for each feature, the models whose page states it in
// the "Supported data types" inputs (image, audio, PDF) or as "Supported" in
// the capabilities row (function calling, structured outputs). The
// gemini-2.5-flash page lists no PDF input, so its document support stays
// unknown. Checked 2026-10-13.
var googleFeatures = map[llm.Feature][]string{
	llm.FeatureImage:            geminiAll,
	llm.FeatureAudio:            geminiAll,
	llm.FeatureDocument:         geminiExcept("gemini-2.5-flash"),
	llm.FeatureTools:            geminiAll,
	llm.FeatureStructuredOutput: geminiAll,
}

func openAIModelsExcept(skip ...string) []string {
	var models []string
	for _, e := range Entries("openai") {
		if !slices.Contains(skip, e.Model) {
			models = append(models, e.Model)
		}
	}
	return models
}

func geminiExcept(skip ...string) []string {
	return slices.DeleteFunc(
		slices.Clone(geminiAll),
		func(m string) bool { return slices.Contains(skip, m) },
	)
}

// geminiClaimPage is the page that states a Gemini model's data types and
// capabilities. The customtools ID is listed under Versions on the
// gemini-3.1-pro-preview page and has no page of its own.
func geminiClaimPage(model string) string {
	if model == "gemini-3.1-pro-preview-customtools" {
		return googleModelPage("gemini-3.1-pro-preview")
	}
	return googleModelPage(model)
}

func openAIModelPage(model string) string {
	return "https://developers.openai.com/api/docs/models/" + model
}

// maxTokensReference is each provider's API reference, which documents an
// output limit parameter for every model the API serves: max_tokens
// (required) on Messages, max_output_tokens on Responses, maxOutputTokens on
// generateContent, and max_tokens on Mistral's chat completions.
var maxTokensReference = map[string]string{
	"anthropic": "https://platform.claude.com/docs/en/api/messages",
	"google":    "https://ai.google.dev/api/generate-content",
	"grok": "https://docs.x.ai/developers/rest-api-reference/" +
		"inference/responses",
	"meta":    "https://dev.meta.ai/docs/protocols/responses",
	"mistral": "https://docs.mistral.ai/api/",
	"openai": "https://developers.openai.com/api/reference/resources/" +
		"responses/methods/create",
}

// outputLimitSources lists the output limits stated on pages other than the
// per-model OpenAI and Google pages, which openAIOutputLimits and
// googleOutputLimits cover. xAI and Meta publish no per-model maximum: the
// Grok 4.7 page states "No text output limit", and Meta's protocol pages
// refer only to "the model's configured maximum".
var outputLimitSources = []outputLimitSource{
	{
		// The model comparison table's "Max output" row.
		url: anthropicOverview,
		models: slices.Concat(
			claudeAdaptiveOnly,
			[]string{"claude-opus-4-6", "claude-sonnet-4-6"},
		),
		limit: 128_000,
	},
	{
		url: anthropicOverview,
		models: []string{
			"claude-opus-4-5",
			"claude-opus-4-5-20251101",
			"claude-sonnet-4-5",
			"claude-sonnet-4-5-20250929",
			"claude-haiku-4-5",
			"claude-haiku-4-5-20251001",
		},
		limit: 64_000,
	},
	{
		// The page's model details table lists both IDs under Versions and
		// states one "Output token limit" for them.
		url: googleModelPage("gemini-3.1-pro-preview"),
		models: []string{
			"gemini-3.1-pro-preview",
			"gemini-3.1-pro-preview-customtools",
		},
		limit: 65_536,
	},
	{
		// "Max output 128k"; the catalog reads k as 1,000, the lower of the
		// two readings. glm-5-2 is the unprefixed alias of the page's ID.
		url:    "https://docs.mistral.ai/models/zai-glm-5-2",
		models: []string{"glm-5-2", "zai-glm-5-2"},
		limit:  128_000,
	},
}

// openAIOutputLimits lists the "max output tokens" each OpenAI model page
// states. gpt-5-codex-mini has no model page, so it has no limit.
var openAIOutputLimits = map[string]int{
	"gpt-6-astra":              128_000,
	"gpt-6.1-sol":              128_000,
	"gpt-6-sol":                128_000,
	"gpt-6-luna":               128_000,
	"gpt-5.6":                  128_000,
	"gpt-5.6-sol":              128_000,
	"gpt-5.6-terra":            128_000,
	"gpt-5.6-luna":             128_000,
	"gpt-5.6-cyber":            128_000,
	"gpt-daybreak-blue-latest": 128_000,
	"gpt-daybreak-red-latest":  128_000,
	"gpt-5.5":                  128_000,
	"gpt-5.5-pro":              128_000,
	"gpt-5.4":                  128_000,
	"gpt-5.4-pro":              128_000,
	"gpt-5.4-mini":             128_000,
	"gpt-5.4-nano":             128_000,
	"gpt-5.2":                  128_000,
	"gpt-5.2-pro":              128_000,
	"gpt-5.1":                  128_000,
	"gpt-5":                    128_000,
	"gpt-5-pro":                272_000,
	"gpt-5-mini":               128_000,
	"gpt-5-nano":               128_000,
	"gpt-4.1":                  32_768,
	"gpt-4.1-mini":             32_768,
	"gpt-4.1-nano":             32_768,
	"gpt-4o":                   16_384,
	"o3":                       100_000,
	"o3-pro":                   100_000,
	"o3-mini":                  100_000,
	"o4-mini":                  100_000,
	"gpt-5.3-codex":            128_000,
}

// googleOutputLimits lists the "Output token limit" each Gemini model page
// states in its model details table.
var googleOutputLimits = map[string]int{
	"gemini-4-argon":         65_536,
	"gemini-3.8-flash":       65_536,
	"gemini-3.7-flash":       65_536,
	"gemini-3.6-flash":       65_536,
	"gemini-3.5-flash":       65_536,
	"gemini-3.5-flash-lite":  65_536,
	"gemini-3.1-flash-lite":  65_536,
	"gemini-3-flash-preview": 65_536,
	"gemini-2.5-pro":         65_536,
	"gemini-2.5-flash":       65_536,
	"gemini-2.5-flash-lite":  65_536,
}

func googleModelPage(model string) string {
	return "https://ai.google.dev/gemini-api/docs/models/" + model
}

// mistralModels is Mistral's models endpoint. Its max_context_length is the
// limit the API enforces; the doc pages round sizes ("256k", "1M"), so
// Mistral context windows come from the endpoint instead.
const mistralModels = "https://api.mistral.ai/v1/models"

// contextWindowSources lists the context windows stated on pages other than
// the per-model OpenAI, Google, Anthropic, and xAI pages, which the tables
// below cover.
var contextWindowSources = []contextWindowSource{
	{
		// The page's model details table lists both IDs under Versions and
		// states one "Input token limit" for them.
		url: googleModelPage("gemini-3.1-pro-preview"),
		models: []string{
			"gemini-3.1-pro-preview",
			"gemini-3.1-pro-preview-customtools",
		},
		window: 1_048_576,
	},
	{
		// The "Available Muse Spark models" table's "Context window" column.
		url: "https://dev.meta.ai/docs/models",
		models: []string{
			"muse-spark-1.3",
			"muse-spark-1.3-contributor",
			"muse-spark-1.2",
			"muse-spark-1.2-contributor",
			"muse-spark-1.1",
		},
		window: 1_048_576,
	},
	{
		// "Muse Wonder takes text, image, video, audio, and PDF input,
		// returns text, has a 1,048,576-token context window".
		url:    "https://dev.meta.ai/docs/models",
		models: []string{"muse-wonder-1.0"},
		window: 1_048_576,
	},
	{
		url: mistralModels,
		models: []string{
			"mistral-medium-latest",
			"magistral-medium-latest",
			"mistral-medium-2604",
			"mistral-medium-3-5",
			"mistral-small-latest",
			"magistral-small-latest",
			"mistral-small-2603",
			"mistral-large-2512",
			"mistral-large-latest",
			"ministral-14b-latest",
			"ministral-14b-2512",
			"ministral-8b-latest",
			"ministral-8b-2512",
		},
		window: 262_144,
	},
	{
		// The changelog and model page say 1M; the endpoint, which the API
		// enforces, reports the smaller value.
		url:    mistralModels,
		models: []string{"mistral-large-4", "mistral-large-4-0"},
		window: 524_288,
	},
	{
		url:    mistralModels,
		models: []string{"ministral-3b-latest", "ministral-3b-2512"},
		window: 131_072,
	},
	{
		url:    mistralModels,
		models: []string{"codestral-latest", "codestral-2508"},
		window: 256_000,
	},
	{
		url:    mistralModels,
		models: []string{"glm-5-2", "zai-glm-5-2"},
		window: 1_048_576,
	},
}

// openAIContextWindows lists the "context window" each OpenAI model page
// states. gpt-5-codex-mini has no model page, so it has no window.
var openAIContextWindows = map[string]int{
	"gpt-6-astra":              1_050_000,
	"gpt-6.1-sol":              1_050_000,
	"gpt-6-sol":                1_050_000,
	"gpt-6-luna":               1_050_000,
	"gpt-5.6":                  1_050_000,
	"gpt-5.6-sol":              1_050_000,
	"gpt-5.6-terra":            1_050_000,
	"gpt-5.6-luna":             1_050_000,
	"gpt-5.6-cyber":            400_000,
	"gpt-daybreak-blue-latest": 1_050_000,
	"gpt-daybreak-red-latest":  400_000,
	"gpt-5.5":                  1_050_000,
	"gpt-5.5-pro":              1_050_000,
	"gpt-5.4":                  1_050_000,
	"gpt-5.4-pro":              1_050_000,
	"gpt-5.4-mini":             400_000,
	"gpt-5.4-nano":             400_000,
	"gpt-5.2":                  400_000,
	"gpt-5.2-pro":              400_000,
	"gpt-5.1":                  400_000,
	"gpt-5":                    400_000,
	"gpt-5-pro":                400_000,
	"gpt-5-mini":               400_000,
	"gpt-5-nano":               400_000,
	"gpt-4.1":                  1_047_576,
	"gpt-4.1-mini":             1_047_576,
	"gpt-4.1-nano":             1_047_576,
	"gpt-4o":                   128_000,
	"o3":                       200_000,
	"o3-pro":                   200_000,
	"o3-mini":                  200_000,
	"o4-mini":                  200_000,
	"gpt-5.3-codex":            400_000,
}

// googleContextWindows lists the "Input token limit" each Gemini model page
// states in its model details table.
var googleContextWindows = map[string]int{
	"gemini-4-argon":         1_048_576,
	"gemini-3.8-flash":       1_048_576,
	"gemini-3.7-flash":       1_048_576,
	"gemini-3.6-flash":       1_048_576,
	"gemini-3.5-flash":       1_048_576,
	"gemini-3.5-flash-lite":  1_048_576,
	"gemini-3.1-flash-lite":  1_048_576,
	"gemini-3-flash-preview": 1_048_576,
	"gemini-2.5-pro":         1_048_576,
	"gemini-2.5-flash":       1_048_576,
	"gemini-2.5-flash-lite":  1_048_576,
}

// anthropicContextWindows lists the "Context window" each Claude model page
// states ("1M tokens", "200K tokens").
var anthropicContextWindows = map[string]int{
	"claude-fable-5-1":           1_000_000,
	"claude-mythos-5-1":          1_000_000,
	"claude-fable-5":             1_000_000,
	"claude-mythos-5":            1_000_000,
	"claude-opus-5-5":            1_000_000,
	"claude-opus-5":              1_000_000,
	"claude-sonnet-5-5":          1_000_000,
	"claude-sonnet-5":            1_000_000,
	"claude-haiku-5-5":           1_000_000,
	"claude-opus-4-8":            1_000_000,
	"claude-opus-4-7":            1_000_000,
	"claude-opus-4-6":            1_000_000,
	"claude-sonnet-4-6":          1_000_000,
	"claude-opus-4-5":            200_000,
	"claude-opus-4-5-20251101":   200_000,
	"claude-sonnet-4-5":          200_000,
	"claude-sonnet-4-5-20250929": 200_000,
	"claude-haiku-4-5":           200_000,
	"claude-haiku-4-5-20251001":  200_000,
}

// anthropicModelPage derives a Claude model's overview page from its ID,
// dropping the "claude-" prefix and any snapshot date.
func anthropicModelPage(model string) string {
	slug := strings.TrimPrefix(model, "claude-")
	if i := strings.LastIndex(slug, "-20"); i > 0 {
		slug = slug[:i]
	}
	return "https://platform.claude.com/docs/en/models/" + slug + "/overview"
}

// grokContextWindows lists the "Context window" each xAI model page states.
var grokContextWindows = map[string]int{
	"grok-4.7":                     500_000,
	"grok-4.6":                     500_000,
	"grok-4.5":                     500_000,
	"grok-4.3":                     1_000_000,
	"grok-4.20-0309-reasoning":     1_000_000,
	"grok-4.20-0309-non-reasoning": 1_000_000,
	"grok-4.20-multi-agent-0309":   1_000_000,
	"grok-build-0.1":               256_000,
}

func grokModelPage(model string) string {
	return "https://docs.x.ai/developers/models/" + model
}

// allClaimSources expands the per-model OpenAI tables and the API-wide
// output limit claim into rows.
func allClaimSources() []claimSource {
	sources := slices.Concat(claimSources, promptCacheClaims(), summaryClaims())
	for _, provider := range Providers() {
		var models []string
		for _, e := range Entries(provider) {
			models = append(models, e.Model)
		}
		sources = append(
			sources,
			claimSource{
				url:     maxTokensReference[provider],
				models:  models,
				feature: llm.FeatureMaxTokens,
				support: llm.SupportSupported,
			},
		)
	}
	for _, model := range slices.Sorted(maps.Keys(openAIEfforts)) {
		sources = append(
			sources,
			claimSource{
				url:     openAIModelPage(model),
				models:  []string{model},
				feature: llm.FeatureReasoningEffort,
				support: llm.SupportSupported,
				efforts: openAIEfforts[model],
			},
		)
	}
	for _, table := range []struct {
		features map[llm.Feature][]string
		page     func(string) string
	}{
		{openAIFeatures, openAIModelPage},
		{googleFeatures, geminiClaimPage},
	} {
		for _, f := range slices.Sorted(maps.Keys(table.features)) {
			for _, model := range table.features[f] {
				sources = append(
					sources,
					claimSource{
						url:     table.page(model),
						models:  []string{model},
						feature: f,
						support: llm.SupportSupported,
					},
				)
			}
		}
	}
	for _, model := range openAINonReasoning {
		sources = append(
			sources,
			claimSource{
				url:     openAIModelPage(model),
				models:  []string{model},
				feature: llm.FeatureReasoningEffort,
				support: llm.SupportUnsupported,
			},
		)
	}
	return sources
}

// allOutputLimitSources expands the per-model OpenAI and Google tables into
// rows.
func allOutputLimitSources() []outputLimitSource {
	sources := slices.Clone(outputLimitSources)
	for _, model := range slices.Sorted(maps.Keys(openAIOutputLimits)) {
		sources = append(
			sources,
			outputLimitSource{
				url:    openAIModelPage(model),
				models: []string{model},
				limit:  openAIOutputLimits[model],
			},
		)
	}
	for _, model := range slices.Sorted(maps.Keys(googleOutputLimits)) {
		sources = append(
			sources,
			outputLimitSource{
				url:    googleModelPage(model),
				models: []string{model},
				limit:  googleOutputLimits[model],
			},
		)
	}
	return sources
}

// allContextWindowSources expands the per-model tables into rows.
func allContextWindowSources() []contextWindowSource {
	sources := slices.Clone(contextWindowSources)
	for _, table := range []struct {
		windows map[string]int
		page    func(string) string
	}{
		{openAIContextWindows, openAIModelPage},
		{googleContextWindows, googleModelPage},
		{anthropicContextWindows, anthropicModelPage},
		{grokContextWindows, grokModelPage},
	} {
		for _, model := range slices.Sorted(maps.Keys(table.windows)) {
			sources = append(
				sources,
				contextWindowSource{
					url:    table.page(model),
					models: []string{model},
					window: table.windows[model],
				},
			)
		}
	}
	return sources
}

func TestEveryClaimNamesTheProviderPageThatStatesIt(t *testing.T) {
	type key struct{ provider, model string }
	wantFeatures := map[key]map[llm.Feature]llm.Support{}
	wantEfforts := map[key][]string{}
	for _, s := range allClaimSources() {
		if s.url == "" {
			t.Fatalf("claim %s has no source page", s.feature)
		}
		for _, model := range s.models {
			k := key{providerOf(t, model), model}
			if wantFeatures[k] == nil {
				wantFeatures[k] = map[llm.Feature]llm.Support{}
			}
			if _, dup := wantFeatures[k][s.feature]; dup {
				t.Fatalf(
					"%s %s is sourced twice",
					model,
					s.feature,
				)
			}
			wantFeatures[k][s.feature] = s.support
			if s.efforts != nil {
				wantEfforts[k] = s.efforts
			}
		}
	}
	for _, provider := range Providers() {
		for _, e := range Entries(provider) {
			k := key{provider, e.Model}
			got := maps.Clone(e.Capabilities.Features)
			if got[llm.FeatureText] != llm.SupportSupported {
				t.Fatalf(
					"%s %s does not claim text output",
					provider,
					e.Model,
				)
			}
			delete(got, llm.FeatureText)
			want := wantFeatures[k]
			for f, support := range got {
				if want[f] != support {
					t.Fatalf(
						"%s %s claims %s %q without a source page",
						provider,
						e.Model,
						f,
						support,
					)
				}
			}
			for f := range want {
				if _, ok := got[f]; !ok {
					t.Fatalf(
						"%s %s lost claim %s",
						provider,
						e.Model,
						f,
					)
				}
			}
			efforts := e.Capabilities.ReasoningEfforts
			if !slices.Equal(efforts, wantEfforts[k]) {
				t.Fatalf(
					"%s %s efforts = %v, sourced %v",
					provider,
					e.Model,
					efforts,
					wantEfforts[k],
				)
			}
		}
	}
}

func TestEveryOutputLimitNamesTheProviderPageThatStatesIt(t *testing.T) {
	type key struct{ provider, model string }
	want := map[key]int{}
	for _, s := range allOutputLimitSources() {
		if s.url == "" || s.limit <= 0 {
			t.Fatalf("output limit %d has no source page", s.limit)
		}
		for _, model := range s.models {
			k := key{providerOf(t, model), model}
			if _, dup := want[k]; dup {
				t.Fatalf("%s output limit is sourced twice", model)
			}
			want[k] = s.limit
		}
	}
	for _, provider := range Providers() {
		for _, e := range Entries(provider) {
			got := e.Capabilities.MaxOutputTokens
			if w := want[key{provider, e.Model}]; got != w {
				t.Fatalf(
					"%s %s max_output_tokens = %d, sourced %d",
					provider,
					e.Model,
					got,
					w,
				)
			}
		}
	}
}

func TestEveryContextWindowNamesTheProviderPageThatStatesIt(t *testing.T) {
	type key struct{ provider, model string }
	want := map[key]int{}
	for _, s := range allContextWindowSources() {
		if s.url == "" || s.window <= 0 {
			t.Fatalf("context window %d has no source page", s.window)
		}
		for _, model := range s.models {
			k := key{providerOf(t, model), model}
			if _, dup := want[k]; dup {
				t.Fatalf("%s context window is sourced twice", model)
			}
			want[k] = s.window
		}
	}
	for _, provider := range Providers() {
		for _, e := range Entries(provider) {
			got := e.Capabilities.ContextWindow
			if w := want[key{provider, e.Model}]; got != w {
				t.Fatalf(
					"%s %s context_window = %d, sourced %d",
					provider,
					e.Model,
					got,
					w,
				)
			}
		}
	}
}

// requestPriceSource is one provider pricing page that states the rate for one
// meter key on the listed models. Every entry of every sheet's requests map
// must appear here, and every row must match the catalog, because a priced key
// is what turns a provider tool request from unpriced into a charge: a wrong
// rate misstates cost, and a missing one hides it. Rates are USD per unit as
// exact decimal text. Free allowances are ignored, so a rate is an upper bound.
type requestPriceSource struct {
	provider string
	meter    string
	models   []string
	rate     string
	url      string
	// checked dates the rate's last check against the page. A sheet's
	// updated_at dates its token rates and is not changed for this.
	checked string
	// verified reports that the unit Dashi counts for the meter is the unit
	// the page bills. When false, note says what is inferred.
	verified bool
	note     string
}

// unpricedMeter is a meter key the catalog deliberately leaves unpriced for
// a provider, with the reason. Models nil means every priced sheet of the
// provider. An unpriced key makes an estimate partial rather than wrong.
type unpricedMeter struct {
	provider string
	meter    string
	models   []string
	reason   string
}

// requestPricesChecked is the date every provider tool request rate was last
// checked against the provider's pricing page.
const requestPricesChecked = "2026-09-28"

const (
	anthropicPricing = "https://platform.claude.com/docs/en/about-claude/" +
		"pricing.md"
	openAIPricing  = "https://developers.openai.com/api/docs/pricing.md"
	openAIMCPGuide = "https://developers.openai.com/api/docs/guides/" +
		"tools-connectors-mcp.md"
	googlePricing = "https://ai.google.dev/gemini-api/docs/pricing"
	grokPricing   = "https://docs.x.ai/developers/pricing"
	metaPricing   = "https://dev.meta.ai/docs/pricing-rate-limits.md"
)

var (
	// openAIReasoningPriced lists the priced OpenAI entries in the gpt-5
	// family, which the pricing page's "reasoning models, including gpt-5,
	// o-series" row covers. The Daybreak aliases point at gpt-5.6 models.
	// gpt-5-codex-mini is not on the pricing page and has no price sheet.
	openAIReasoningPriced = []string{
		"gpt-6-astra",
		"gpt-6.1-sol",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.6-cyber",
		"gpt-daybreak-blue-latest",
		"gpt-daybreak-red-latest",
		"gpt-5.5",
		"gpt-5.5-pro",
		"gpt-5.4",
		"gpt-5.4-pro",
		"gpt-5.4-mini",
		"gpt-5.4-nano",
		"gpt-5.2",
		"gpt-5.2-pro",
		"gpt-5.1",
		"gpt-5",
		"gpt-5-pro",
		"gpt-5-mini",
		"gpt-5-nano",
		"o3",
		"o3-pro",
		"o3-mini",
		"o4-mini",
		"gpt-5.3-codex",
	}
	// openAINonReasoningPriced lists the priced OpenAI entries without a
	// reasoning step.
	openAINonReasoningPriced = []string{
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"gpt-4o",
	}
	openAIPriced = slices.Concat(
		openAIReasoningPriced,
		openAINonReasoningPriced,
	)
	// gemini3AndLater lists the Gemini 3 and later entries, whose pricing
	// rows bill "search
	// requests" at $14 per 1,000.
	gemini3AndLater = []string{
		"gemini-4-argon",
		"gemini-3.8-flash",
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
		"gemini-3.5-flash-lite",
		"gemini-3.1-pro-preview",
		"gemini-3.1-pro-preview-customtools",
		"gemini-3.1-flash-lite",
		"gemini-3-flash-preview",
	}
	// gemini25 lists the Gemini 2.5 entries, whose pricing rows bill
	// "grounded prompts" at $35 per 1,000.
	gemini25 = []string{
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-2.5-flash-lite",
	}
	geminiAll = slices.Concat(gemini3AndLater, gemini25)
	grokChat  = []string{
		"grok-4.7",
		"grok-4.6",
		"grok-4.5",
		"grok-4.3",
		"grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-multi-agent-0309",
		"grok-build-0.1",
	}
	museText = []string{
		"muse-wonder-1.0",
		"muse-spark-1.3",
		"muse-spark-1.3-contributor",
		"muse-spark-1.2",
		"muse-spark-1.2-contributor",
		"muse-spark-1.1",
	}
)

var requestPriceSources = []requestPriceSource{
	{
		// "$10 per 1,000 searches"; the response reports
		// usage.server_tool_use.web_search_requests. The page states one
		// rate for the Claude API, so the limited-access Mythos models,
		// which its model table lists, share it.
		provider: "anthropic",
		meter:    llm.MeterWebSearch,
		models:   claudeAll,
		rate:     "0.01",
		url:      anthropicPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "Web fetch usage has no additional charges beyond standard token
		// costs".
		provider: "anthropic",
		meter:    llm.MeterWebFetch,
		models:   claudeAll,
		rate:     "0",
		url:      anthropicPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "Web search (all models) $10.00 / 1k calls". gpt-4.1-mini is left
		// unpriced; see unpricedMeters.
		provider: "openai",
		meter:    llm.MeterWebSearch,
		models: slices.DeleteFunc(
			slices.Clone(openAIPriced),
			func(m string) bool {
				return m == "gpt-4.1-mini"
			},
		),
		rate:    "0.01",
		url:     openAIPricing,
		checked: requestPricesChecked,
		note: "Counting only web_search_call items with action " +
			"\"search\" as billed calls is a reading of the docs.",
	},
	{
		// "Web search preview (reasoning models, including gpt-5,
		// o-series) $10.00 / 1k calls".
		provider: "openai",
		meter:    llm.MeterWebSearchPreview,
		models:   openAIReasoningPriced,
		rate:     "0.01",
		url:      openAIPricing,
		checked:  requestPricesChecked,
		note: "Counting only web_search_call items with action " +
			"\"search\" as billed calls is a reading of the docs.",
	},
	{
		// "Web search preview (non-reasoning models) $25.00 / 1k calls".
		provider: "openai",
		meter:    llm.MeterWebSearchPreview,
		models:   openAINonReasoningPriced,
		rate:     "0.025",
		url:      openAIPricing,
		checked:  requestPricesChecked,
		note: "Counting only web_search_call items with action " +
			"\"search\" as billed calls is a reading of the docs.",
	},
	{
		// "File search: Tool call $2.50 / 1k calls"; storage is billed
		// separately and is not a request.
		provider: "openai",
		meter:    llm.MeterFileSearch,
		models:   openAIPriced,
		rate:     "0.0025",
		url:      openAIPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "No additional fees apply per tool call".
		provider: "openai",
		meter:    llm.MeterMCP,
		models:   openAIPriced,
		rate:     "0",
		url:      openAIMCPGuide,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "5,000 free search requests per month ... then $14 per 1,000
		// requests."
		provider: "google",
		meter:    llm.MeterWebSearch,
		models:   gemini3AndLater,
		rate:     "0.014",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// Gemini 3.x bills per search request, not per grounded prompt.
		provider: "google",
		meter:    llm.MeterGroundedPrompt,
		models:   gemini3AndLater,
		rate:     "0",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "1,500 RPD (free), then $35 / 1,000 grounded prompts".
		provider: "google",
		meter:    llm.MeterGroundedPrompt,
		models:   gemini25,
		rate:     "0.035",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// Gemini 2.5 bills per grounded prompt, not per search query.
		provider: "google",
		meter:    llm.MeterWebSearch,
		models:   gemini25,
		rate:     "0",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// URL context, code execution, and file search are billed as
		// tokens only; the page lists no per-request charge.
		provider: "google",
		meter:    llm.MeterURLContext,
		models:   geminiAll,
		rate:     "0",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		provider: "google",
		meter:    llm.MeterCodeExecution,
		models:   geminiAll,
		rate:     "0",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		provider: "google",
		meter:    llm.MeterFileSearch,
		models:   geminiAll,
		rate:     "0",
		url:      googlePricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "Web Search web_search $5 / 1k calls".
		provider: "grok",
		meter:    llm.MeterWebSearch,
		models:   grokChat,
		rate:     "0.005",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "Code Execution code_execution $5 / 1k calls".
		provider: "grok",
		meter:    llm.MeterCodeExecution,
		models:   grokChat,
		rate:     "0.005",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "Collections Search collections_search file_search $2.50 / 1k
		// calls".
		provider: "grok",
		meter:    llm.MeterFileSearch,
		models:   grokChat,
		rate:     "0.0025",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "X Search is billed per item fetched": $5 / 1k posts.
		provider: "grok",
		meter:    llm.MeterXSearchPost,
		models:   grokChat,
		rate:     "0.005",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// $10 / 1k profiles.
		provider: "grok",
		meter:    llm.MeterXSearchProfile,
		models:   grokChat,
		rate:     "0.01",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "For Remote MCP tools, you will not be charged for the tool
		// invocation but will be charged for any tokens used."
		provider: "grok",
		meter:    llm.MeterMCP,
		models:   grokChat,
		rate:     "0",
		url:      grokPricing,
		checked:  requestPricesChecked,
		verified: true,
	},
	{
		// "$2.50 per 1,000 search queries ... on text models such as Muse
		// Spark", and Muse Wonder. Muse Image includes search in its
		// per-image price and has no catalog entry.
		provider: "meta",
		meter:    llm.MeterWebSearch,
		models:   museText,
		rate:     "0.0025",
		url:      metaPricing,
		checked:  requestPricesChecked,
		note: "Meta bills per search query; counting one query per " +
			"web_search_call item is unverified.",
	},
}

var unpricedMeters = []unpricedMeter{
	{
		provider: "anthropic",
		meter:    llm.MeterCodeExecution,
		reason: "Billed per container-hour with a 5-minute minimum, and " +
			"the response reports no duration. It is also free when " +
			"web_search_20260209 or web_fetch_20260209 or later is in the " +
			"request, and Dashi sends older versions.",
	},
	{
		provider: "anthropic",
		meter:    llm.MeterMCP,
		reason:   "The pricing page publishes no rate for MCP connector calls.",
	},
	{
		provider: "openai",
		meter:    llm.MeterCodeExecution,
		reason: "Billed per 20-minute container session by memory size, " +
			"not per call.",
	},
	{
		provider: "openai",
		meter:    llm.MeterImageGeneration,
		reason:   "Billed as image tokens at the image model's rates.",
	},
	{
		provider: "openai",
		meter:    llm.MeterWebSearch,
		models:   []string{"gpt-4.1-mini"},
		reason: "Search content is billed as a fixed block of 8,000 input " +
			"tokens per call that the usage does not report, so a per-call " +
			"rate alone would understate it. gpt-4o-mini has the same rule " +
			"but no catalog entry.",
	},
	{
		provider: "grok",
		meter:    llm.MeterImageGeneration,
		reason: "Billed at Imagine API per-image rates, which vary by " +
			"model.",
	},
}

// emittedMeters lists every meter key each provider's adapters can count
// into Usage.Requests. Mistral's provider connectors exist only in its
// Conversations API, which Dashi does not use.
var emittedMeters = map[string][]string{
	"anthropic": {
		llm.MeterWebSearch,
		llm.MeterWebFetch,
		llm.MeterCodeExecution,
		llm.MeterMCP,
	},
	"openai": {
		llm.MeterWebSearch,
		llm.MeterWebSearchPreview,
		llm.MeterFileSearch,
		llm.MeterCodeExecution,
		llm.MeterImageGeneration,
		llm.MeterMCP,
	},
	"google": {
		llm.MeterWebSearch,
		llm.MeterGroundedPrompt,
		llm.MeterURLContext,
		llm.MeterCodeExecution,
		llm.MeterFileSearch,
	},
	"grok": {
		llm.MeterWebSearch,
		llm.MeterXSearchPost,
		llm.MeterXSearchProfile,
		llm.MeterCodeExecution,
		llm.MeterFileSearch,
		llm.MeterImageGeneration,
		llm.MeterMCP,
	},
	"meta": {
		llm.MeterWebSearch,
	},
	"mistral": nil,
}

func TestEveryRequestPriceNamesTheProviderPageThatStatesIt(t *testing.T) {
	type key struct{ provider, model string }
	want := map[key]map[string]money.Amount{}
	for _, s := range requestPriceSources {
		if s.url == "" || s.checked != requestPricesChecked {
			t.Fatalf(
				"%s %s row needs a url and checked date %s",
				s.provider,
				s.meter,
				requestPricesChecked,
			)
		}
		if !s.verified && s.note == "" {
			t.Fatalf(
				"%s %s is unverified without a note",
				s.provider,
				s.meter,
			)
		}
		rate, err := money.ParseAmount(s.rate)
		if err != nil {
			t.Fatalf("%s %s: %v", s.provider, s.meter, err)
		}
		for _, model := range s.models {
			if p := providerOf(t, model); p != s.provider {
				t.Fatalf(
					"%s row names %s model %s",
					s.provider,
					p,
					model,
				)
			}
			k := key{s.provider, model}
			if want[k] == nil {
				want[k] = map[string]money.Amount{}
			}
			if _, dup := want[k][s.meter]; dup {
				t.Fatalf("%s %s is sourced twice", model, s.meter)
			}
			want[k][s.meter] = rate
		}
	}
	for _, provider := range Providers() {
		for _, e := range Entries(provider) {
			var got map[string]money.Amount
			if e.Pricing != nil {
				got = e.Pricing.Requests
			}
			w := want[key{provider, e.Model}]
			for meter, rate := range got {
				if sourced, ok := w[meter]; !ok || sourced != rate {
					t.Fatalf(
						"%s %s prices %s at %s without a matching row",
						provider,
						e.Model,
						meter,
						rate,
					)
				}
			}
			for meter, rate := range w {
				if _, ok := got[meter]; !ok {
					t.Fatalf(
						"%s %s lost %s, sourced at %s",
						provider,
						e.Model,
						meter,
						rate,
					)
				}
			}
		}
	}
}

// TestEveryEmittedMeterIsPricedOrExplained keeps a new adapter meter or a new
// sheet from silently leaving provider tool work unpriced: each key an adapter
// can count is priced on every priced sheet of its provider or listed in
// unpricedMeters with a reason.
func TestEveryEmittedMeterIsPricedOrExplained(t *testing.T) {
	unpriced := func(provider, meter, model string) bool {
		return slices.ContainsFunc(
			unpricedMeters,
			func(u unpricedMeter) bool {
				return u.provider == provider &&
					u.meter == meter &&
					(u.models == nil || slices.Contains(u.models, model))
			},
		)
	}
	for _, u := range unpricedMeters {
		if u.reason == "" {
			t.Fatalf(
				"%s %s is unpriced without a reason",
				u.provider,
				u.meter,
			)
		}
		if !slices.Contains(emittedMeters[u.provider], u.meter) {
			t.Fatalf(
				"%s never emits unpriced meter %s",
				u.provider,
				u.meter,
			)
		}
		for _, model := range u.models {
			if p := providerOf(t, model); p != u.provider {
				t.Fatalf(
					"%s unpriced row names %s model %s",
					u.provider,
					p,
					model,
				)
			}
		}
	}
	for _, provider := range Providers() {
		emitted, ok := emittedMeters[provider]
		if !ok {
			t.Fatalf("provider %s has no emitted meter list", provider)
		}
		for _, e := range Entries(provider) {
			if e.Pricing == nil {
				continue
			}
			for meter := range e.Pricing.Requests {
				if !slices.Contains(emitted, meter) {
					t.Fatalf(
						"%s %s prices %s, which no adapter emits",
						provider,
						e.Model,
						meter,
					)
				}
				if unpriced(provider, meter, e.Model) {
					t.Fatalf(
						"%s %s prices %s, listed as unpriced",
						provider,
						e.Model,
						meter,
					)
				}
			}
			for _, meter := range emitted {
				_, priced := e.Pricing.Requests[meter]
				if !priced && !unpriced(provider, meter, e.Model) {
					t.Fatalf(
						"%s %s leaves %s unpriced without a reason",
						provider,
						e.Model,
						meter,
					)
				}
			}
		}
	}
}

// providerOf finds the catalog provider for a sourced model, so a row that
// names a model the catalog lacks fails instead of being ignored.
func providerOf(t *testing.T, model string) string {
	t.Helper()
	for _, provider := range Providers() {
		if _, ok := Find(provider, model); ok {
			return provider
		}
	}
	t.Fatalf("sourced model %q is not in the catalog", model)
	return ""
}
