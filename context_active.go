package dashi

import (
	"errors"
	"github.com/deepnoodle-ai/dashi/llm"
	"math"
	"slices"
)

// ActiveCompactionPolicy bounds model context during one Turn. Summaries use
// that Turn's model attempts and budget. The byte estimate is not a tokenizer.
type ActiveCompactionPolicy struct {
	Disabled  bool    `json:"disabled,omitempty"`
	Window    int     `json:"window,omitempty"`
	Fraction  float64 `json:"fraction,omitempty"`
	MaxTokens int     `json:"max_tokens,omitempty"`
}

func (p *ActiveCompactionPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.Window < 0 || p.Window > 0 && p.Window < 1024 || p.MaxTokens < 0 || math.IsNaN(p.Fraction) || math.IsInf(p.Fraction, 0) || p.Fraction < 0 || p.Fraction > 1 {
		return errors.New("dashi: invalid active compaction policy")
	}
	return nil
}
func (p *ActiveCompactionPolicy) resolved() *ActiveCompactionPolicy {
	if p == nil {
		return nil
	}
	q := *p
	if q.Fraction == 0 {
		q.Fraction = .8
	}
	if q.MaxTokens == 0 {
		q.MaxTokens = 2048
	}
	return &q
}

// ModelPurpose identifies internal summary work. Empty means ordinary work.
type ModelPurpose string

const ModelPurposeCompaction ModelPurpose = "compaction"

// TurnMessageRef identifies a projected message within a source step.
type TurnMessageRef struct {
	Seq   int64 `json:"seq"`
	Index int   `json:"index"`
}

// ContextCompactionPlan binds a summary attempt to complete settled exchanges.
type ContextCompactionPlan struct {
	Through         int64            `json:"through"`
	Covered         []TurnMessageRef `json:"covered"`
	Digest          string           `json:"digest"`
	Window          int              `json:"window"`
	EstimatedBefore int              `json:"estimated_before"`
}

// ContextCompactionFact records the successfully applied summary and estimate.
type ContextCompactionFact struct {
	ModelCallID    string `json:"model_call_id"`
	Summary        string `json:"summary"`
	EstimatedAfter int    `json:"estimated_after"`
}

func (p *ContextCompactionPlan) clone() *ContextCompactionPlan {
	if p == nil {
		return nil
	}
	q := *p
	q.Covered = slices.Clone(p.Covered)
	return &q
}

// activeWindow returns the context window a Start records for active
// compaction, or 0 for none. It reads the model's window from the client, so
// it needs one whenever active compaction is on.
func activeWindow(a Agent, client llm.Client, origin StartOrigin) (int, error) {
	p := a.ActiveCompaction
	if p == nil || p.Disabled || origin == StartCompaction {
		return 0, nil
	}
	if client == nil {
		return 0, errors.New("dashi: client is required to find the model's context window for active compaction")
	}
	info, known := client.ModelInfo(a.Model)
	window := 0
	if known {
		window = info.Capabilities.ContextWindow
	}
	if p.Window > 0 {
		if window <= 0 || p.Window < window {
			window = p.Window
		}
	}
	if window < 1024 {
		return 0, nil
	}
	return window, nil
}
