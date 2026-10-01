package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/model"
)

// ModelRef is retained as an alias for existing model-call clients.
type ModelRef = model.ModelRef

// Support is retained as an alias for existing model-call clients.
type Support = model.Support

const (
	// SupportUnknown means no support claim has been established.
	SupportUnknown = model.SupportUnknown
	// SupportSupported means capability support has been established.
	SupportSupported = model.SupportSupported
	// SupportUnsupported means known rejection for this capability.
	SupportUnsupported = model.SupportUnsupported
)

// Feature names a model behavior relevant to request preparation.
type Feature string

// Features are checked when the request actually uses them.
const (
	// FeatureText describes the base output capability required by every model
	// call.
	FeatureText Feature = "text"
	// FeatureImage describes support for image inputs, including tool output.
	FeatureImage Feature = "image"
	// FeatureDocument describes support for document inputs, including tool
	// output.
	FeatureDocument Feature = "document"
	// FeatureAudio describes support for audio inputs, including tool output.
	FeatureAudio Feature = "audio"
	// FeatureResource describes support for resource inputs, including tool
	// output.
	FeatureResource Feature = "resource"
	// FeatureTools describes support for tool definitions, calls, and results.
	FeatureTools Feature = "tools"
	// FeatureToolChoice describes support for an explicit tool-selection
	// constraint.
	FeatureToolChoice Feature = "tool_choice"
	// FeatureParallelTools describes support for an explicit parallel-tool
	// setting.
	FeatureParallelTools Feature = "parallel_tools"
	// FeatureTemperature describes support for an explicit sampling
	// temperature.
	FeatureTemperature Feature = "temperature"
	// FeatureMaxTokens describes support for an explicit output token limit.
	FeatureMaxTokens Feature = "max_tokens"
	// FeatureReasoningEffort describes support for reasoning controlled by
	// effort level.
	FeatureReasoningEffort Feature = "reasoning_effort"
	// FeatureReasoningBudget describes support for reasoning controlled by
	// token budget.
	FeatureReasoningBudget Feature = "reasoning_budget"
	// FeatureReasoningSummary describes native opt-in to public reasoning summaries.
	FeatureReasoningSummary Feature = "reasoning_summary"
	// FeatureStreaming describes support for streamed responses.
	FeatureStreaming Feature = "streaming"
	// FeatureCitations describes support for citing request documents. It is
	// requested when Settings.Citations is set and the request carries a
	// document. APIs that cannot request citations declare it unsupported,
	// and Fit omits the setting; elsewhere it is checked like any feature.
	FeatureCitations Feature = "citations"
	// FeatureStructuredOutput describes native JSON Schema response formatting.
	FeatureStructuredOutput Feature = "structured_output"
	// FeaturePromptCache describes support for requesting a prompt cache:
	// cache points on Anthropic, a lifetime elsewhere. It is requested when
	// Settings.PromptCache.TTL is set. APIs that cannot request a lifetime
	// declare it unsupported, and Fit omits the TTL with a warning.
	FeaturePromptCache Feature = "prompt_cache"
)

// ProviderToolFeature scopes model evidence to a provider's wire tool type.
// ModelEvidence already binds that fact to one API and endpoint.
func ProviderToolFeature(toolType string) Feature {
	return Feature("provider_tool:" + toolType)
}

// CapabilityPolicy is shared with media operations so one application setting
// means the same thing for every model-backed call.
type CapabilityPolicy = model.CapabilityPolicy

const (
	// CapabilityAdjust is the default: it sends unknown support and fits
	// known limits to the nearest supported value with a warning.
	CapabilityAdjust = model.CapabilityAdjust
	// CapabilityStrict declines unknown support and out-of-range values.
	CapabilityStrict = model.CapabilityStrict
)

// Capabilities describes model behavior, independently of pricing. Zero limits
// and empty effort lists mean no known constraint, not an unlimited model.
// Features only describe this library's vocabulary, not the full remote API.
type Capabilities struct {
	// Features maps requested behaviors to known support; missing keys mean
	// unknown.
	Features map[Feature]Support `json:"features,omitempty"`

	// MaxOutputTokens is a known output limit; zero means no known constraint.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	// ContextWindow is the documented context size in tokens; zero means
	// unknown. Providers differ on whether output counts against it, so treat
	// it as an upper bound on input. Preparation does not enforce it, because
	// input cannot be counted offline.
	ContextWindow int `json:"context_window,omitempty"`

	// ReasoningEfforts lists accepted levels when known; an empty list adds no
	// constraint.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`

	// MinReasoningBudget is a known lower bound; zero adds no constraint.
	MinReasoningBudget int `json:"min_reasoning_budget,omitempty"`

	// MaxReasoningBudget is a known upper bound; zero adds no constraint.
	MaxReasoningBudget int `json:"max_reasoning_budget,omitempty"`

	// MaxTemperature is a known upper bound on sampling temperature; zero
	// adds no constraint.
	MaxTemperature float64 `json:"max_temperature,omitempty"`

	// CacheTTLs lists the prompt cache lifetimes the route offers, shortest
	// first. The shortest is the route's default, which needs no request
	// field. Empty means unknown and adds no constraint.
	CacheTTLs Durations `json:"cache_ttls,omitempty"`

	// CacheKeyDefault allows a session to send its ID as the prompt cache
	// key when the agent sets none. It is set where a key per conversation
	// improves cache routing without separating shared prefixes.
	CacheKeyDefault bool `json:"cache_key_default,omitempty"`
}

// Check validates requested features and known limits without changing the
// request; call Fit first to bring settings within limits. Under
// CapabilityAdjust the returned features have unknown support, so callers can
// record that uncertainty. Known rejection fails under both policies.
// Input.Settings should contain effective settings when checking defaults.
func (c Capabilities) Check(
	r Input,
	policy CapabilityPolicy,
) ([]Feature, error) {
	if !policy.Valid() {
		return nil, fmt.Errorf(
			"llm: invalid capability policy %q",
			policy,
		)
	}
	var unverified []Feature
	for _, f := range requestedFeatures(r) {
		switch c.Features[f] {
		case SupportSupported:
		case SupportUnknown:
			if policy == CapabilityStrict {
				return nil, fmt.Errorf(
					"llm: model %q support for %s is unknown; "+
						"add ModelInfo evidence for it or use the adjust capability policy",
					r.Model,
					f,
				)
			}
			unverified = append(unverified, f)
		case SupportUnsupported:
			return nil, unsupportedError(r.Model, f)
		default:
			return nil, fmt.Errorf(
				"llm: invalid support value for %s",
				f,
			)
		}
	}
	if _, _, err := c.Fit(r, CapabilityStrict, nil); err != nil {
		return nil, err
	}
	return unverified, nil
}

// ModelInfo holds claims supplied by the application or a catalog. Evidence
// identifies their scope and provenance; Prepare never upgrades their quality.
// Pricing has its own date. Neither snapshot guarantees API acceptance.
type ModelInfo struct {
	// Capabilities holds the behavior described by this evidence.
	Capabilities Capabilities `json:"capabilities"`

	// Pricing is an optional estimate with its own update date; nil means
	// unknown.
	Pricing *Pricing `json:"pricing,omitempty"`

	// Evidence scopes the capabilities to the exact model and destination.
	Evidence ModelEvidence `json:"evidence"`
}

// Clone returns a snapshot without shared maps, slices, or pricing data.
func (m ModelInfo) Clone() ModelInfo {
	m.Capabilities.Features = maps.Clone(m.Capabilities.Features)
	m.Capabilities.ReasoningEfforts = slices.Clone(m.Capabilities.ReasoningEfforts)
	m.Capabilities.CacheTTLs = slices.Clone(m.Capabilities.CacheTTLs)
	if m.Pricing != nil {
		p := m.Pricing.Clone()
		m.Pricing = &p
	}
	return m
}

// Validate requires dated evidence and coherent constraints. It does not
// claim the supplied evidence is accurate or current.
func (m ModelInfo) Validate() error {
	if err := m.Evidence.Validate(); err != nil {
		return err
	}
	for f, support := range m.Capabilities.Features {
		if !knownFeature(f) || !support.Valid() {
			return fmt.Errorf(
				"llm: invalid capability %q: %q",
				f,
				support,
			)
		}
	}
	c := m.Capabilities
	if c.MaxOutputTokens < 0 || c.ContextWindow < 0 ||
		c.MinReasoningBudget < 0 || c.MaxReasoningBudget < 0 ||
		(c.MaxReasoningBudget > 0 &&
			c.MinReasoningBudget > c.MaxReasoningBudget) ||
		!(c.MaxTemperature >= 0) || math.IsInf(c.MaxTemperature, 0) {
		return errors.New("llm: invalid capability limits")
	}
	for i, ttl := range c.CacheTTLs {
		if ttl <= 0 || (i > 0 && ttl <= c.CacheTTLs[i-1]) {
			return errors.New(
				"llm: cache ttls must be positive and increasing",
			)
		}
	}
	if m.Pricing != nil {
		if err := m.Pricing.Validate(); err != nil {
			return err
		}
	}
	_, err := json.Marshal(m)
	return err
}

func knownFeature(f Feature) bool {
	switch f {
	case FeatureText, FeatureImage, FeatureDocument, FeatureAudio, FeatureResource,
		FeatureTools, FeatureToolChoice, FeatureParallelTools, FeatureTemperature, FeatureMaxTokens,
		FeatureReasoningEffort, FeatureReasoningBudget, FeatureReasoningSummary, FeatureStreaming,
		FeatureCitations, FeatureStructuredOutput, FeaturePromptCache:
		return true
	}
	return strings.HasPrefix(string(f), "provider_tool:") &&
		strings.TrimPrefix(string(f), "provider_tool:") != ""
}

func requestedFeatures(r Input) []Feature {
	// Every call requests model output. Text is the base output capability.
	features := []Feature{FeatureText}
	add := func(f Feature) {
		if !slices.Contains(features, f) {
			features = append(features, f)
		}
	}
	data := func(b content.Block) {
		switch b.Kind {
		case content.KindImage:
			add(FeatureImage)
		case content.KindDocument:
			add(FeatureDocument)
		case content.KindAudio:
			add(FeatureAudio)
		case content.KindResource:
			add(FeatureResource)
		}
	}
	if r.Stream {
		add(FeatureStreaming)
	}
	if len(r.OutputSchema) > 0 {
		add(FeatureStructuredOutput)
	}
	if len(r.Tools) > 0 {
		add(FeatureTools)
		for _, definition := range r.Tools {
			if definition.ProviderTool != nil {
				add(ProviderToolFeature(definition.ProviderTool.Type))
			}
		}
	}
	if r.Settings.ToolChoice != nil {
		add(FeatureToolChoice)
	}
	if r.Settings.ParallelTools != nil {
		add(FeatureParallelTools)
	}
	if r.Settings.Temperature != nil {
		add(FeatureTemperature)
	}
	if r.Settings.MaxTokens > 0 {
		add(FeatureMaxTokens)
	}
	// Citations change the request only when it carries a document.
	if r.Settings.Citations && hasDocument(r) {
		add(FeatureCitations)
	}
	if r.Settings.PromptCache.TTL > 0 {
		add(FeaturePromptCache)
	}
	if s := r.Settings.ReasoningSummary; s != nil && *s {
		add(FeatureReasoningSummary)
	}
	if s := r.Settings.Reasoning; s != nil {
		if s.Effort != "" {
			add(FeatureReasoningEffort)
		} else {
			add(FeatureReasoningBudget)
		}
	}
	for _, m := range r.Messages {
		for _, part := range m.Content {
			if part.Block != nil {
				data(*part.Block)
			}
			if part.ToolCall != nil || part.ToolResult != nil ||
				part.ProviderToolUse != nil ||
				part.ProviderToolResult != nil ||
				part.ProviderToolApprovalRequest != nil ||
				part.ProviderToolApprovalResponse != nil {
				add(FeatureTools)
			}
			if part.ToolResult != nil {
				for _, b := range part.ToolResult.Output.Content {
					data(b)
				}
			}
			// Hosted outputs are display projections of provider replay data.
			// They are not encoded as independent media inputs.
		}
	}
	return features
}
