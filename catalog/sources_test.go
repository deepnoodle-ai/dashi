package catalog

import (
	"maps"
	"slices"
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
	"gpt-5.2-codex": {"low", "medium", "high", "xhigh"},
}

// openAINonReasoning names models whose page says they have no reasoning
// step ("non-reasoning model", "without a reasoning step").
var openAINonReasoning = []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano"}

func openAIModelPage(model string) string {
	return "https://developers.openai.com/api/docs/models/" + model
}

// allClaimSources expands the per-model OpenAI tables into rows.
func allClaimSources() []claimSource {
	sources := slices.Clone(claimSources)
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
