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

// HostedActivityPolicy controls omission of completed foreign hosted work.
type HostedActivityPolicy string

const (
	// HostedReject preserves the strict default for provider-owned work.
	HostedReject HostedActivityPolicy = ""
	// HostedOmit permits recorded omission of complete, paired foreign work.
	HostedOmit HostedActivityPolicy = "omit"
)

// ContentPolicy selects specific content losses for one input. It never
// authorizes dropping unknown opaque content or private data on other kinds.
type ContentPolicy struct {
	// UnreplayableReasoning permits omission only when reasoning cannot be
	// represented at the selected destination.
	UnreplayableReasoning ReasoningPolicy `json:"unreplayable_reasoning,omitempty"`

	// ForeignHostedActivity permits omission of complete provider-owned work
	// only when its private replay cannot reach the selected destination.
	ForeignHostedActivity HostedActivityPolicy `json:"foreign_hosted_activity,omitempty"`
}

// Validate rejects unknown loss policies rather than silently ignoring them.
func (p ContentPolicy) Validate() error {
	switch p.UnreplayableReasoning {
	case ReasoningReject, ReasoningOmit:
	default:
		return errors.New("llm: unsupported unreplayable reasoning policy")
	}
	switch p.ForeignHostedActivity {
	case HostedReject, HostedOmit:
		return nil
	default:
		return errors.New("llm: unsupported foreign hosted activity policy")
	}
}
