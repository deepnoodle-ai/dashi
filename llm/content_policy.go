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

// ContentPolicy selects specific content losses for one input. It never
// authorizes dropping unknown opaque content or private data on other kinds.
type ContentPolicy struct {
	// UnreplayableReasoning permits omission only when reasoning cannot be
	// represented at the selected destination.
	UnreplayableReasoning ReasoningPolicy `json:"unreplayable_reasoning,omitempty"`
}

// Validate rejects unknown loss policies rather than silently ignoring them.
func (p ContentPolicy) Validate() error {
	switch p.UnreplayableReasoning {
	case ReasoningReject, ReasoningOmit:
		return nil
	default:
		return errors.New("llm: unsupported unreplayable reasoning policy")
	}
}
