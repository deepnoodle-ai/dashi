package llm

import "errors"

// ReasoningPolicy selects how preparation handles reasoning that cannot be
// replayed to the selected destination.
type ReasoningPolicy string

const (
	// ReasoningReject preserves the strict default.
	ReasoningReject ReasoningPolicy = ""
	// ReasoningOmit permits dropping only recognized, unreplayable reasoning.
	ReasoningOmit ReasoningPolicy = "omit"
)

// ProviderToolActivityPolicy controls omission of completed foreign hosted work.
type ProviderToolActivityPolicy string

const (
	// ProviderToolReject preserves the strict default for provider-owned work.
	ProviderToolReject ProviderToolActivityPolicy = ""
	// ProviderToolOmit permits recorded omission of complete, paired foreign work.
	ProviderToolOmit ProviderToolActivityPolicy = "omit"
)

// ContentPolicy selects specific content losses for one input. It never
// authorizes dropping unknown opaque content or private data on other kinds.
type ContentPolicy struct {
	// UnreplayableReasoning permits omission only when reasoning cannot be
	// represented at the selected destination.
	UnreplayableReasoning ReasoningPolicy `json:"unreplayable_reasoning,omitempty"`

	// ForeignProviderToolActivity permits omission of complete provider-owned work
	// only when its private replay cannot reach the selected destination.
	ForeignProviderToolActivity ProviderToolActivityPolicy `json:"foreign_provider_tool_activity,omitempty"`
}

// Validate rejects unknown loss policies rather than silently ignoring them.
func (p ContentPolicy) Validate() error {
	switch p.UnreplayableReasoning {
	case ReasoningReject, ReasoningOmit:
	default:
		return errors.New("llm: unsupported unreplayable reasoning policy")
	}
	switch p.ForeignProviderToolActivity {
	case ProviderToolReject, ProviderToolOmit:
		return nil
	default:
		return errors.New("llm: unsupported foreign provider tool activity policy")
	}
}
