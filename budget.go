package kernel

import (
	"errors"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/deepnoodle-ai/kernel/llm"
)

// Money is one amount in one currency. Budgets do not convert currencies.
type Money struct {
	// Amount is denominated in Currency without conversion.
	Amount float64 `json:"amount"`
	// Currency must match the price sheet used for cost admission.
	Currency string `json:"currency"`
}

// Budget caps a turn's cumulative model usage and model-call charges. Start
// sets it; Continue may raise existing caps after host authorization.
type Budget struct {
	// MaxOutputTokens caps generated tokens across all calls in the turn.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// MaxTotalTokens caps disjoint input buckets plus output across calls.
	MaxTotalTokens int `json:"max_total_tokens,omitempty"`
	// MaxCost requires every model-call charge in the same currency.
	MaxCost *Money `json:"max_cost,omitempty"`
}

// Clone separates the mutable cost cap from the caller's budget.
func (b Budget) Clone() Budget {
	if b.MaxCost != nil {
		cost := *b.MaxCost
		b.MaxCost = &cost
	}
	return b
}

// Validate rejects invalid caps before a budget is recorded on a turn.
func (b Budget) Validate() error {
	if b.MaxOutputTokens < 0 || b.MaxTotalTokens < 0 {
		return errors.New("kernel: budget token caps cannot be negative")
	}
	if b.MaxCost != nil && (strings.TrimSpace(b.MaxCost.Currency) == "" ||
		strings.TrimSpace(b.MaxCost.Currency) != b.MaxCost.Currency ||
		b.MaxCost.Amount <= 0 || math.IsNaN(b.MaxCost.Amount) ||
		math.IsInf(b.MaxCost.Amount, 0)) {
		return errors.New(
			"kernel: budget cost cap requires a positive finite amount and currency",
		)
	}
	return nil
}

// BudgetRaise preserves each authorized increase for replay and audit.
type BudgetRaise struct {
	// Budget is the complete effective budget after the raise.
	Budget Budget `json:"budget"`
	// At is the recorded time of the raise.
	At time.Time `json:"at"`
}

// Clone separates the raised cap from the source audit record.
func (r BudgetRaise) Clone() BudgetRaise {
	r.Budget = r.Budget.Clone()
	return r
}

// LimitKind identifies the limit that stopped a turn.
type LimitKind string

const (
	// LimitModelAttempts is the definition's model call ceiling.
	LimitModelAttempts LimitKind = "model_attempts"
	// LimitDeadline is the definition's time ceiling.
	LimitDeadline LimitKind = "deadline"
	// LimitInterruptions is the definition's resume ceiling.
	LimitInterruptions LimitKind = "interruptions"
	// LimitProviderPauses is the definition's provider pause ceiling.
	LimitProviderPauses LimitKind = "provider_pauses"
	// LimitOutputTokens is the turn's cumulative output cap.
	LimitOutputTokens LimitKind = "output_tokens"
	// LimitTotalTokens is the turn's cumulative input and output cap.
	LimitTotalTokens LimitKind = "total_tokens"
	// LimitCost is the turn's cumulative cost cap.
	LimitCost LimitKind = "cost"
	// LimitHost is an application-owned spending decision.
	LimitHost LimitKind = "host"
)

// LimitDetail gives machine-readable amounts and an application code for a
// host limit. FitTokens is advice, not an automatic request change.
type LimitDetail struct {
	// Kind identifies the ceiling for programmatic handling.
	Kind LimitKind `json:"kind"`
	// Code is the application's stable identifier for a host limit.
	Code string `json:"code,omitempty"`
	// Limit is the ceiling in the units selected by Kind.
	Limit float64 `json:"limit,omitempty"`
	// Used is the measured or estimated amount already consumed.
	Used float64 `json:"used,omitempty"`
	// AtRisk is the worst-case cost of the proposed next model call.
	AtRisk float64 `json:"at_risk,omitempty"`
	// FitTokens advises a smaller allowance without changing the request.
	FitTokens int `json:"fit_tokens,omitempty"`
	// UnpricedModel identifies the model whose cost could not be bounded.
	UnpricedModel string `json:"unpriced_model,omitempty"`
}

// Validate checks the typed stop metadata independently of its prose reason.
func (l LimitDetail) Validate() error {
	switch l.Kind {
	case LimitModelAttempts, LimitDeadline, LimitInterruptions,
		LimitProviderPauses, LimitOutputTokens, LimitTotalTokens, LimitCost:
		if l.Code != "" {
			return errors.New("kernel: only host limits carry a code")
		}
	case LimitHost:
		if l.Code == "" {
			return errors.New("kernel: host limit requires a code")
		}
	default:
		return errors.New("kernel: invalid limit kind")
	}
	for _, amount := range []float64{l.Limit, l.Used, l.AtRisk} {
		if amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return errors.New("kernel: limit amounts must be finite and nonnegative")
		}
	}
	if l.FitTokens < 0 {
		return errors.New("kernel: fit tokens cannot be negative")
	}
	return nil
}

// Spend is reconstructed from committed model and tool settlements. MaxCost
// applies to Cost, while ToolCost is reported separately for host admission.
// Estimated counts calls without final usage, including partial streamed
// frames; CostUnknown counts calls whose model charge is unknown or in another
// currency.
type Spend struct {
	// Usage aggregates disjoint token buckets across attempted model calls.
	Usage llm.Usage `json:"usage"`
	// Cost contains model-call charges by currency, without conversion.
	Cost map[string]float64 `json:"cost,omitempty"`
	// ToolCost contains local tool charges outside MaxCost.
	ToolCost map[string]float64 `json:"tool_cost,omitempty"`
	// Estimated counts calls without complete provider-reported usage.
	Estimated int `json:"estimated,omitempty"`
	// CostUnknown counts attempts whose charge cannot enforce a cost cap.
	CostUnknown int `json:"cost_unknown,omitempty"`
}

// Clone copies the mutable usage and currency totals for independent use.
func (s Spend) Clone() Spend {
	s.Usage = *s.Usage.Copy()
	s.Cost = maps.Clone(s.Cost)
	s.ToolCost = maps.Clone(s.ToolCost)
	return s
}

func (b Budget) raisedFrom(old Budget) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.MaxOutputTokens < old.MaxOutputTokens ||
		b.MaxTotalTokens < old.MaxTotalTokens {
		return errors.New("kernel: continuation budget cannot lower token caps")
	}
	if old.MaxOutputTokens == 0 && b.MaxOutputTokens != 0 ||
		old.MaxTotalTokens == 0 && b.MaxTotalTokens != 0 ||
		old.MaxCost == nil && b.MaxCost != nil {
		return errors.New("kernel: continuation cannot introduce a new cap")
	}
	if old.MaxCost != nil {
		if b.MaxCost == nil || b.MaxCost.Currency != old.MaxCost.Currency ||
			b.MaxCost.Amount < old.MaxCost.Amount {
			return errors.New(
				"kernel: continuation budget cannot lower or change cost cap",
			)
		}
	}
	if b.MaxOutputTokens == old.MaxOutputTokens &&
		b.MaxTotalTokens == old.MaxTotalTokens &&
		((b.MaxCost == nil && old.MaxCost == nil) ||
			(b.MaxCost != nil && old.MaxCost != nil &&
				b.MaxCost.Amount == old.MaxCost.Amount)) {
		return errors.New("kernel: continuation budget must raise a cap")
	}
	return nil
}

func (s Spend) costIn(currency string) (float64, bool) {
	used := s.Cost[currency]
	if s.CostUnknown > 0 {
		return used, false
	}
	for unit, amount := range s.Cost {
		if unit != currency && amount != 0 {
			return used, false
		}
	}
	return used, true
}

func addCurrencyCost(
	totals map[string]float64,
	currency string,
	amount float64,
) error {
	total := totals[currency] + amount
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return errors.New("kernel: cumulative cost overflows")
	}
	totals[currency] = total
	return nil
}

func cloneBudget(b *Budget) *Budget {
	if b == nil {
		return nil
	}
	copy := b.Clone()
	return &copy
}
