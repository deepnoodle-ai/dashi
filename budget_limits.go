package dashi

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func checkSendBudget(
	s *replayState,
	request llm.Request,
	model string,
) (*llm.Cost, *Money, *Stop) {
	if stop := incompleteUsageStop(s); stop != nil {
		return nil, nil, stop
	}
	snapshot := request.Snapshot()
	var atRisk *llm.Cost
	if snapshot.ModelInfo != nil && snapshot.ModelInfo.Pricing != nil {
		if value, ok := llm.WorstCaseCost(request, *snapshot.ModelInfo.Pricing); ok {
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
			Kind:   StopLimited,
			Reason: uncheckableCostReason(s.Spend),
			Limit: &LimitDetail{
				Kind:      LimitCost,
				CostLimit: cap.Amount,
				Currency:  cap.Currency,
				CostUsed:  used,
			},
		}
	}
	if atRisk == nil || atRisk.Currency != cap.Currency {
		reason := fmt.Sprintf(
			"cost risk unavailable for model %s; the %s %s cost cap "+
				"cannot be enforced",
			model,
			cap.Amount,
			cap.Currency,
		)
		unpriced := ""
		if snapshot.ModelInfo == nil || snapshot.ModelInfo.Pricing == nil {
			reason = fmt.Sprintf(
				"no price for model %s; the %s %s cost cap cannot "+
					"be enforced; supply this model's prices to run capped turns",
				model,
				cap.Amount,
				cap.Currency,
			)
			unpriced = model
		} else if snapshot.Effective.MaxTokens <= 0 {
			reason = fmt.Sprintf(
				"cost risk unavailable for model %s: a finite max_tokens "+
					"allowance is required to enforce the %s %s cost cap",
				model,
				cap.Amount,
				cap.Currency,
			)
		} else if hasProviderTools(request) {
			reason = fmt.Sprintf(
				"cost risk unavailable for model %s: provider tools "+
					"can bill an unbounded number of requests and add "+
					"unbounded input tokens within one call; remove provider "+
					"tools to enforce the %s %s cost cap, or run the turn "+
					"without a cost cap",
				model,
				cap.Amount,
				cap.Currency,
			)
		}
		return atRisk, nil, &Stop{
			Kind:   StopLimited,
			Reason: reason,
			Limit: &LimitDetail{
				Kind:          LimitCost,
				CostLimit:     cap.Amount,
				Currency:      cap.Currency,
				CostUsed:      used,
				UnpricedModel: unpriced,
			},
		}
	}
	remaining := cap.Amount - used
	// A sum past the int64 range is over any cap; it must not wrap.
	if after, ok := used.Add(atRisk.Total); !ok || after > cap.Amount {
		fit := llm.FitOutputTokens(request, *snapshot.ModelInfo.Pricing, remaining)
		return atRisk, nil, &Stop{
			Kind: StopLimited,
			Reason: costLimitReason(
				cap.Amount,
				used,
				atRisk.Total,
				snapshot.Effective.MaxTokens,
				fit,
				cap.Currency,
			),
			Limit: &LimitDetail{
				Kind:                     LimitCost,
				CostLimit:                cap.Amount,
				Currency:                 cap.Currency,
				CostUsed:                 used,
				WorstCaseCost:            atRisk.Total,
				SuggestedMaxOutputTokens: fit,
			},
		}
	}
	return atRisk, &Money{Amount: remaining, Currency: cap.Currency}, nil
}

func costLimitReason(
	cap money.Amount,
	used money.Amount,
	risk money.Amount,
	tokens int,
	fit int,
	currency string,
) string {
	if fit == 0 {
		return fmt.Sprintf(
			"next call could cost up to %s %s (%d output tokens); "+
				"%s of the %s %s cap remains; no positive output-token "+
				"allowance would fit; raise the cap or reduce input/history",
			risk,
			currency,
			tokens,
			max(cap-used, 0),
			cap,
			currency,
		)
	}
	return fmt.Sprintf(
		"next call could cost up to %s %s (%d output tokens); "+
			"%s of the %s %s cap remains; an output-token "+
			"allowance of about %d would fit",
		risk,
		currency,
		tokens,
		max(cap-used, 0),
		cap,
		currency,
		fit,
	)
}

// uncheckableCostReason explains why recorded spend cannot be compared with
// the cost cap, naming each cause present, and what the caller can do.
func uncheckableCostReason(spend Spend) string {
	var reserved, priceable []string
	for _, key := range slices.Sorted(maps.Keys(spend.UnpricedRequests)) {
		if llm.IsReservedMeter(key) {
			reserved = append(reserved, key)
		} else {
			priceable = append(priceable, key)
		}
	}
	var causes []string
	if len(priceable) > 0 {
		causes = append(causes, fmt.Sprintf(
			"a model call billed provider tool requests with no price (%s); "+
				"supply prices for them or remove the tools that cause them",
			strings.Join(priceable, ", "),
		))
	}
	if len(reserved) > 0 {
		causes = append(causes, fmt.Sprintf(
			"a model call ran provider tool work whose billing could not be "+
				"attributed (%s)",
			strings.Join(reserved, ", "),
		))
	}
	// Under a cap every partial charge also counts as unknown, so any
	// excess is a missing charge or one in another currency.
	if spend.CostUnknown > spend.CostPartial || len(causes) == 0 {
		causes = append(
			causes,
			"an earlier charge is unknown or in another currency",
		)
	}
	return "cost cannot be checked: " + strings.Join(causes, "; ") +
		"; this turn cannot continue under its cost cap; start a new " +
		"turn with this turn's history"
}

func hasProviderTools(request llm.Request) bool {
	for _, def := range request.Input().Tools {
		if def.Kind == tool.KindProviderTool {
			return true
		}
	}
	return false
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
	if stop := incompleteUsageStop(s); stop != nil {
		return stop
	}
	b := s.Budget
	output := s.Spend.Usage.OutputTokens
	if b.MaxOutputTokens > 0 &&
		(output > b.MaxOutputTokens ||
			!overOnly && output == b.MaxOutputTokens) {
		return &Stop{
			Kind:   StopLimited,
			Reason: string(LimitOutputTokens),
			Limit: &LimitDetail{
				Kind:  LimitOutputTokens,
				Limit: int64(b.MaxOutputTokens),
				Used:  int64(output),
			},
		}
	}
	total := s.Spend.Usage.TotalInputTokens() + s.Spend.Usage.OutputTokens
	if b.MaxTotalTokens > 0 && (total > b.MaxTotalTokens ||
		!overOnly && total == b.MaxTotalTokens) {
		return &Stop{
			Kind:   StopLimited,
			Reason: string(LimitTotalTokens),
			Limit: &LimitDetail{
				Kind:  LimitTotalTokens,
				Limit: int64(b.MaxTotalTokens),
				Used:  int64(total),
			},
		}
	}
	if b.MaxCost == nil {
		return nil
	}
	used, known := s.Spend.costIn(b.MaxCost.Currency)
	if !known {
		return &Stop{
			Kind:   StopLimited,
			Reason: uncheckableCostReason(s.Spend),
			Limit: &LimitDetail{
				Kind:      LimitCost,
				CostLimit: b.MaxCost.Amount,
				Currency:  b.MaxCost.Currency,
				CostUsed:  used,
			},
		}
	}
	if used > b.MaxCost.Amount ||
		!overOnly && used == b.MaxCost.Amount {
		return &Stop{
			Kind:   StopLimited,
			Reason: string(LimitCost),
			Limit: &LimitDetail{
				Kind:      LimitCost,
				CostLimit: b.MaxCost.Amount,
				Currency:  b.MaxCost.Currency,
				CostUsed:  used,
			},
		}
	}
	return nil
}

func tokenCapped(b *Budget) bool {
	return b != nil && (b.MaxOutputTokens > 0 || b.MaxTotalTokens > 0)
}

func incompleteUsageStop(s *replayState) *Stop {
	if !s.Spend.UsageIncomplete || !tokenCapped(s.Budget) {
		return nil
	}
	limit := &LimitDetail{
		Kind:  LimitTotalTokens,
		Limit: int64(s.Budget.MaxTotalTokens),
		Used: int64(s.Spend.Usage.TotalInputTokens() +
			s.Spend.Usage.OutputTokens),
	}
	if s.Budget.MaxOutputTokens > 0 {
		limit.Kind = LimitOutputTokens
		limit.Limit = int64(s.Budget.MaxOutputTokens)
		limit.Used = int64(s.Spend.Usage.OutputTokens)
	}
	return &Stop{
		Kind: StopLimited,
		Reason: "usage aggregation is incomplete; this token cap cannot " +
			"be enforced; resolve outstanding effects as required, then " +
			"start a new turn with prior history",
		Limit: limit,
	}
}
