package catalog

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
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
	anthropicToolUse = "https://platform.claude.com/docs/en/agents-and-tools/" +
		"tool-use/overview"
	anthropicSonnet45 = "https://platform.claude.com/docs/en/models/" +
		"sonnet-4-5/overview"
	anthropicHaiku45 = "https://platform.claude.com/docs/en/models/" +
		"haiku-4-5/overview"
)

var (
	claudeAll = []string{
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-5-5",
		"claude-opus-5",
		"claude-sonnet-5",
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
		"claude-sonnet-5",
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
		// The pricing table lists tool_choice any and tool for these
		// models; forced choice is rejected on Opus 5.5 and the 5.1 models.
		url: anthropicToolUse,
		models: []string{
			"claude-opus-5",
			"claude-sonnet-5",
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
		url: mistralModels,
		models: []string{
			"mistral-medium-latest",
			"magistral-medium-latest",
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
	"claude-sonnet-5":            1_000_000,
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
	sources := slices.Clone(claimSources)
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
