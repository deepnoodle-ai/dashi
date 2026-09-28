package kernel

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/llm"
)

// Money is one amount in one currency. Budgets do not convert currencies.
type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Budget caps a turn's cumulative model usage and cost. Start sets it; a
// Continue command may raise existing caps after host authorization.
type Budget struct {
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int    `json:"max_total_tokens,omitempty"`
	MaxCost         *Money `json:"max_cost,omitempty"`
}

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

// BudgetRaise records the effective budget and the time of an authorized raise.
type BudgetRaise struct {
	Budget Budget    `json:"budget"`
	At     time.Time `json:"at"`
}

// LimitKind identifies the limit that stopped a turn.
type LimitKind string

const (
	LimitModelAttempts  LimitKind = "model_attempts"
	LimitDeadline       LimitKind = "deadline"
	LimitInterruptions  LimitKind = "interruptions"
	LimitProviderPauses LimitKind = "provider_pauses"
	LimitOutputTokens   LimitKind = "output_tokens"
	LimitTotalTokens    LimitKind = "total_tokens"
	LimitCost           LimitKind = "cost"
	LimitHost           LimitKind = "host"
)

// LimitDetail gives machine-readable amounts and an application code for a
// host limit. FitTokens is advice, not an automatic request change.
type LimitDetail struct {
	Kind          LimitKind `json:"kind"`
	Code          string    `json:"code,omitempty"`
	Limit         float64   `json:"limit,omitempty"`
	Used          float64   `json:"used,omitempty"`
	AtRisk        float64   `json:"at_risk,omitempty"`
	FitTokens     int       `json:"fit_tokens,omitempty"`
	UnpricedModel string    `json:"unpriced_model,omitempty"`
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

// Spend is reconstructed from committed model and tool settlements. Estimated
// counts calls whose usage never arrived; CostUnknown counts calls whose charge
// cannot be expressed in the turn's cost-cap currency (or has no charge).
type Spend struct {
	Usage       llm.Usage          `json:"usage"`
	Cost        map[string]float64 `json:"cost,omitempty"`
	Estimated   int                `json:"estimated,omitempty"`
	CostUnknown int                `json:"cost_unknown,omitempty"`
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

func checkSendBudget(
	s *replayState,
	request llm.Request,
	model string,
) (*llm.Cost, *Money, *Stop) {
	snapshot := request.Snapshot()
	var atRisk *llm.Cost
	if snapshot.ModelInfo != nil && snapshot.ModelInfo.Pricing != nil {
		if value, ok := llm.MaxCostAtRisk(
			request, *snapshot.ModelInfo.Pricing,
		); ok {
			atRisk = &value
		}
	}
	if s.Budget == nil || s.Budget.MaxCost == nil {
		return atRisk, nil, nil
	}
	cap := s.Budget.MaxCost
	used, known := s.Spend.costIn(cap.Currency)
	if !known {
		return atRisk, nil, &Stop{
			Kind: StopLimited,
			Reason: "cost cannot be checked: an earlier charge is unknown " +
				"or in another currency",
			Limit: &LimitDetail{
				Kind:  LimitCost,
				Limit: cap.Amount,
				Used:  used,
			},
		}
	}
	if atRisk == nil || atRisk.Currency != cap.Currency {
		reason := fmt.Sprintf(
			"cost risk unavailable for model %s; the %.6g %s cost cap "+
				"cannot be enforced",
			model, cap.Amount, cap.Currency,
		)
		unpriced := ""
		if snapshot.ModelInfo == nil || snapshot.ModelInfo.Pricing == nil {
			reason = fmt.Sprintf(
				"no price for model %s; the %.6g %s cost cap cannot "+
					"be enforced; supply this model's prices to run capped turns",
				model, cap.Amount, cap.Currency,
			)
			unpriced = model
		}
		return atRisk, nil, &Stop{
			Kind:   StopLimited,
			Reason: reason,
			Limit: &LimitDetail{
				Kind:          LimitCost,
				Limit:         cap.Amount,
				Used:          used,
				UnpricedModel: unpriced,
			},
		}
	}
	remaining := cap.Amount - used
	if used+atRisk.Total > cap.Amount {
		fit := llm.FitOutputTokens(
			request, *snapshot.ModelInfo.Pricing, remaining,
		)
		return atRisk, nil, &Stop{
			Kind: StopLimited,
			Reason: costLimitReason(
				cap.Amount, used, atRisk.Total,
				snapshot.Effective.MaxTokens, fit, cap.Currency,
			),
			Limit: &LimitDetail{
				Kind:      LimitCost,
				Limit:     cap.Amount,
				Used:      used,
				AtRisk:    atRisk.Total,
				FitTokens: fit,
			},
		}
	}
	return atRisk, &Money{Amount: remaining, Currency: cap.Currency}, nil
}

func costLimitReason(
	cap, used, risk float64,
	tokens, fit int,
	currency string,
) string {
	if fit == 0 {
		return fmt.Sprintf(
			"next call could cost up to %.6g %s (%d output tokens); "+
				"%.6g of the %.6g %s cap remains; even zero output "+
				"tokens would not fit",
			risk, currency, tokens, math.Max(0, cap-used), cap, currency,
		)
	}
	return fmt.Sprintf(
		"next call could cost up to %.6g %s (%d output tokens); "+
			"%.6g of the %.6g %s cap remains; an output-token "+
			"allowance of about %d would fit",
		risk, currency, tokens, math.Max(0, cap-used), cap, currency,
		fit,
	)
}

func attemptSpend(
	intent, settlement *ModelFact,
) (*llm.Usage, *llm.Cost, bool, bool) {
	if settlement != nil && (settlement.Failure == llm.FailureRejected ||
		settlement.Failure == llm.FailureNotDispatched || settlement.NoDispatch) {
		return nil, nil, false, false
	}
	var usage *llm.Usage
	var reported *llm.Cost
	if settlement != nil && settlement.Result != nil {
		usage = settlement.Result.Usage
		reported = settlement.Result.Cost
	}
	estimated := usage == nil
	if estimated {
		guess := &llm.Usage{InputTokens: intent.EstimatedInputTokens}
		if settlement != nil && settlement.Result != nil {
			text := settlement.Result.Message.Text()
			guess.OutputTokens = (utf8.RuneCountInString(text) + 3) / 4
		}
		usage = guess
	} else {
		usage = usage.Copy()
	}
	if settlement != nil && settlement.Result != nil {
		observed := map[string]int{}
		for _, part := range settlement.Result.Message.Content {
			if part.Kind != llm.ContentHostedUse ||
				part.HostedUse == nil {
				continue
			}
			name := part.HostedUse.Name
			observed[name]++
		}
		if len(observed) > 0 && usage.Requests == nil {
			usage.Requests = map[string]int{}
		}
		for name, count := range observed {
			usage.Requests[name] = max(usage.Requests[name], count)
		}
	}
	if settlement != nil && settlement.Charge != nil {
		return usage, settlement.Charge, estimated, true
	}
	if reported != nil {
		return usage, reported, estimated, true
	}
	if intent.ModelInfo != nil && intent.ModelInfo.Pricing != nil {
		if charge, ok := intent.ModelInfo.Pricing.Estimate(usage); ok {
			return usage, &charge, estimated, true
		}
	}
	return usage, nil, estimated, true
}

func setSettlementCharge(intent, settlement *ModelFact) {
	if intent == nil || settlement == nil {
		return
	}
	_, charge, estimated, counted := attemptSpend(intent, settlement)
	if counted {
		settlement.Charge = charge
		settlement.EstimatedUsage = estimated
	}
}

func budgetSpentStop(s *replayState) *Stop {
	return budgetStop(s, false)
}

func budgetOvershotStop(s *replayState) *Stop {
	return budgetStop(s, true)
}

func budgetStop(s *replayState, overOnly bool) *Stop {
	if s.Budget == nil {
		return nil
	}
	b := s.Budget
	output := s.Spend.Usage.OutputTokens
	if b.MaxOutputTokens > 0 &&
		(output > b.MaxOutputTokens ||
			!overOnly && output == b.MaxOutputTokens) {
		return &Stop{Kind: StopLimited, Reason: "output_tokens",
			Limit: &LimitDetail{Kind: LimitOutputTokens,
				Limit: float64(b.MaxOutputTokens),
				Used:  float64(s.Spend.Usage.OutputTokens)}}
	}
	total := s.Spend.Usage.TotalInputTokens() + s.Spend.Usage.OutputTokens
	if b.MaxTotalTokens > 0 && (total > b.MaxTotalTokens ||
		!overOnly && total == b.MaxTotalTokens) {
		return &Stop{Kind: StopLimited, Reason: "total_tokens",
			Limit: &LimitDetail{Kind: LimitTotalTokens,
				Limit: float64(b.MaxTotalTokens), Used: float64(total)}}
	}
	if b.MaxCost == nil {
		return nil
	}
	used, known := s.Spend.costIn(b.MaxCost.Currency)
	if !known {
		return &Stop{Kind: StopLimited,
			Reason: "cost cannot be checked: an earlier charge is unknown or in another currency",
			Limit: &LimitDetail{Kind: LimitCost, Limit: b.MaxCost.Amount,
				Used: used}}
	}
	if used > b.MaxCost.Amount ||
		!overOnly && used == b.MaxCost.Amount {
		return &Stop{Kind: StopLimited, Reason: "cost",
			Limit: &LimitDetail{Kind: LimitCost,
				Limit: b.MaxCost.Amount, Used: used}}
	}
	return nil
}
