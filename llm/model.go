package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/model"
)

// ModelRef is retained as an alias for existing model-call clients.
type ModelRef = model.ModelRef

// Support is retained as an alias for existing model-call clients.
type Support = model.Support

const (
	SupportUnknown     = model.SupportUnknown
	SupportSupported   = model.SupportSupported
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
	// FeatureStreaming describes support for streamed responses.
	FeatureStreaming Feature = "streaming"
)

// HostedToolFeature scopes model evidence to a provider's wire tool type.
// ModelEvidence already binds that fact to one API and endpoint.
func HostedToolFeature(toolType string) Feature {
	return Feature("hosted_tool:" + toolType)
}

// UnknownPolicy is retained as an alias for existing model-call clients.
type UnknownPolicy = model.UnknownPolicy

const (
	UnknownReject = model.UnknownReject
	UnknownAllow  = model.UnknownAllow
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

	// ReasoningEfforts lists accepted levels when known; an empty list adds no
	// constraint.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`

	// MinReasoningBudget is a known lower bound; zero adds no constraint.
	MinReasoningBudget int `json:"min_reasoning_budget,omitempty"`

	// MaxReasoningBudget is a known upper bound; zero adds no constraint.
	MaxReasoningBudget int `json:"max_reasoning_budget,omitempty"`
}

// Check validates requested features and known limits. Its returned features
// are unverified under UnknownAllow, so callers can record that uncertainty.
// Input.Settings should contain effective settings when checking defaults.
func (c Capabilities) Check(r Input, policy UnknownPolicy) ([]Feature, error) {
	if policy != UnknownReject && policy != UnknownAllow {
		return nil, fmt.Errorf("llm: invalid unknown capability policy %q", policy)
	}
	var unverified []Feature
	for _, f := range requestedFeatures(r) {
		switch c.Features[f] {
		case SupportSupported:
		case SupportUnknown:
			if policy == UnknownReject {
				return nil, fmt.Errorf("llm: model %q support for %s is unknown", r.Model, f)
			}
			unverified = append(unverified, f)
		case SupportUnsupported:
			return nil, fmt.Errorf("llm: model %q does not support %s", r.Model, f)
		default:
			return nil, fmt.Errorf("llm: invalid support value for %s", f)
		}
	}
	if c.MaxOutputTokens > 0 && r.Settings.MaxTokens > c.MaxOutputTokens {
		return nil, fmt.Errorf("llm: max_tokens exceeds model limit %d", c.MaxOutputTokens)
	}
	if s := r.Settings.Reasoning; s != nil {
		if s.Effort != "" && len(c.ReasoningEfforts) > 0 && !slices.Contains(c.ReasoningEfforts, s.Effort) {
			return nil, fmt.Errorf("llm: reasoning effort %q is unsupported", s.Effort)
		}
		if s.BudgetTokens > 0 && (s.BudgetTokens < c.MinReasoningBudget || (c.MaxReasoningBudget > 0 && s.BudgetTokens > c.MaxReasoningBudget)) {
			return nil, errors.New("llm: reasoning budget is outside model limits")
		}
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
		if !knownFeature(f) || (support != SupportUnknown && support != SupportSupported && support != SupportUnsupported) {
			return fmt.Errorf("llm: invalid capability %q: %q", f, support)
		}
	}
	c := m.Capabilities
	if c.MaxOutputTokens < 0 || c.MinReasoningBudget < 0 || c.MaxReasoningBudget < 0 || (c.MaxReasoningBudget > 0 && c.MinReasoningBudget > c.MaxReasoningBudget) {
		return errors.New("llm: invalid capability limits")
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
		FeatureReasoningEffort, FeatureReasoningBudget, FeatureStreaming:
		return true
	}
	return strings.HasPrefix(string(f), "hosted_tool:") &&
		strings.TrimPrefix(string(f), "hosted_tool:") != ""
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
	if len(r.Tools) > 0 {
		add(FeatureTools)
		for _, definition := range r.Tools {
			if definition.Hosted != nil {
				add(HostedToolFeature(definition.Hosted.Type))
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
			if part.ToolCall != nil || part.ToolResult != nil {
				add(FeatureTools)
			}
			if part.ToolResult != nil {
				for _, b := range part.ToolResult.Output.Content {
					data(b)
				}
			}
		}
	}
	return features
}
