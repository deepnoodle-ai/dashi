package dashi

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/money"
)

// Money is one exact amount in one currency. Budgets do not convert
// currencies.
type Money struct {
	// Amount is denominated in Currency without conversion.
	Amount money.Amount `json:"amount"`
	// Currency must match the price sheet used for cost reservations.
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
		return errors.New("dashi: budget token caps cannot be negative")
	}
	if b.MaxCost != nil && (strings.TrimSpace(b.MaxCost.Currency) == "" ||
		strings.TrimSpace(b.MaxCost.Currency) != b.MaxCost.Currency ||
		b.MaxCost.Amount <= 0) {
		return errors.New(
			"dashi: budget cost cap requires a positive amount and currency",
		)
	}
	return nil
}

// BudgetRaise preserves each authorized increase, so rebuilding the turn's
// state and audits see it.
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
	// LimitModelCalls is the agent's model call ceiling.
	LimitModelCalls LimitKind = "model_calls"
	// LimitContextWindow stops a request that cannot fit its context
	// window. It ends the turn: start a successor turn, for example after
	// compaction.
	LimitContextWindow LimitKind = "context_window"
	// LimitDeadline is the agent's time ceiling.
	LimitDeadline LimitKind = "deadline"
	// LimitResumes is the agent's resume ceiling.
	LimitResumes LimitKind = "resumes"
	// LimitProviderPauses is the agent's provider pause ceiling.
	LimitProviderPauses LimitKind = "provider_pauses"
	// LimitOutputTokens is the turn's cumulative output cap.
	LimitOutputTokens LimitKind = "output_tokens"
	// LimitTotalTokens is the turn's cumulative input and output cap.
	LimitTotalTokens LimitKind = "total_tokens"
	// LimitCost is the turn's cumulative cost cap.
	LimitCost LimitKind = "cost"
	// LimitHost is an application-owned spending decision. A host denial
	// puts money, such as a balance and the amount the call needs, in the
	// Cost fields with a Currency, and uses Limit and Used only for counts.
	LimitHost LimitKind = "host"
)

// LimitDetail gives machine-readable amounts and an application code for a
// host limit. SuggestedMaxOutputTokens is advice, not an automatic request change.
//
// Counts and money are separate fields so that neither passes through a
// binary float: Limit and Used hold whole counts, and the Cost fields hold
// exact amounts in Currency.
type LimitDetail struct {
	// Kind identifies the ceiling for programmatic handling.
	Kind LimitKind `json:"kind"`
	// Code is the application's stable identifier for a host limit.
	Code string `json:"code,omitempty"`
	// Limit is the ceiling for a counted kind: tokens, model calls,
	// resumes, or provider pauses, or Unix seconds for a deadline.
	Limit int64 `json:"limit,omitempty"`
	// Used is the count already consumed, or the current Unix time. A token
	// count is only a lower bound when Spend.UsageIncomplete is true or
	// Spend.CallsWithoutFinalUsage is nonzero.
	Used int64 `json:"used,omitempty"`
	// CostLimit is the cost cap for LimitCost, or a host's money ceiling or
	// balance for LimitHost.
	CostLimit money.Amount `json:"cost_limit,omitempty"`
	// CostUsed is the cost already charged from reported usage.
	CostUsed money.Amount `json:"cost_used,omitempty"`
	// WorstCaseCost is the worst-case cost of the proposed next model call.
	WorstCaseCost money.Amount `json:"worst_case_cost,omitempty"`
	// Currency is the unit of the Cost fields and is required when any of
	// them is nonzero: an ISO 4217 code, or a host's own code for a credit
	// unit. Dashi sets the cap's currency on LimitCost.
	Currency string `json:"currency,omitempty"`
	// SuggestedMaxOutputTokens advises a smaller allowance without changing the request.
	SuggestedMaxOutputTokens int `json:"suggested_max_output_tokens,omitempty"`
	// UnpricedModel identifies the model whose cost could not be bounded.
	UnpricedModel string `json:"unpriced_model,omitempty"`
	// Ends means this host limit ends the turn, such as a routine's
	// spending cap: Stop.Ended is set and Next gives NextNone. Without it
	// a host limit waits (NextFix) until the host puts a Continue in the
	// inbox once the cause clears, such as a budget top-up. Only LimitHost
	// may set it.
	Ends bool `json:"ends,omitempty"`
}

// Validate checks the typed stop metadata independently of its prose reason.
func (l LimitDetail) Validate() error {
	switch l.Kind {
	case LimitContextWindow, LimitModelCalls, LimitDeadline, LimitResumes,
		LimitProviderPauses, LimitOutputTokens, LimitTotalTokens, LimitCost:
		if l.Code != "" {
			return errors.New("dashi: only host limits carry a code")
		}
		if l.Ends {
			return errors.New("dashi: only host limits set Ends")
		}
	case LimitHost:
		if l.Code == "" {
			return errors.New("dashi: host limit requires a code")
		}
	default:
		return errors.New("dashi: invalid limit kind")
	}
	if l.Limit < 0 || l.Used < 0 ||
		l.CostLimit < 0 || l.CostUsed < 0 || l.WorstCaseCost < 0 {
		return errors.New("dashi: limit amounts must be nonnegative")
	}
	if l.SuggestedMaxOutputTokens < 0 {
		return errors.New("dashi: fit tokens cannot be negative")
	}
	hasMoney := l.CostLimit != 0 || l.CostUsed != 0 || l.WorstCaseCost != 0
	if strings.TrimSpace(l.Currency) != l.Currency ||
		hasMoney && l.Currency == "" {
		return errors.New("dashi: limit amounts require a currency")
	}
	return nil
}

func (b Budget) raisedFrom(old Budget) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.MaxOutputTokens < old.MaxOutputTokens ||
		b.MaxTotalTokens < old.MaxTotalTokens {
		return errors.New("dashi: continuation budget cannot lower token caps")
	}
	if old.MaxOutputTokens == 0 && b.MaxOutputTokens != 0 ||
		old.MaxTotalTokens == 0 && b.MaxTotalTokens != 0 ||
		old.MaxCost == nil && b.MaxCost != nil {
		return errors.New("dashi: continuation cannot introduce a new cap")
	}
	if old.MaxCost != nil {
		if b.MaxCost == nil || b.MaxCost.Currency != old.MaxCost.Currency ||
			b.MaxCost.Amount < old.MaxCost.Amount {
			return errors.New(
				"dashi: continuation budget cannot lower or change cost cap",
			)
		}
	}
	if b.MaxOutputTokens == old.MaxOutputTokens &&
		b.MaxTotalTokens == old.MaxTotalTokens &&
		((b.MaxCost == nil && old.MaxCost == nil) ||
			(b.MaxCost != nil && old.MaxCost != nil &&
				b.MaxCost.Amount == old.MaxCost.Amount)) {
		return errors.New("dashi: continuation budget must raise a cap")
	}
	return nil
}

func cloneBudget(b *Budget) *Budget {
	if b == nil {
		return nil
	}
	copy := b.Clone()
	return &copy
}

// LimitError is the error a Recorder returns, inside a CommitError with
// NothingWritten set, to refuse a StepModelCallStarted step because the
// call would break a limit the recorder enforces, such as a balance checked
// in the same transaction that reserves the call's
// CommitRequest.ModelCall.WorstCaseCost. The runner then records a limited
// stop with Reason and Limit, as if a BeforeSend hook had returned it, so
// the race between a hook's check and the reservation never leaves a turn
// without a stop. Limit must be a host, cost, or token limit.
type LimitError struct {
	Reason string
	Limit  LimitDetail
}

func (e *LimitError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("dashi: %s limit: %s", e.Limit.Kind, e.Reason)
	}
	return fmt.Sprintf("dashi: %s limit", e.Limit.Kind)
}
