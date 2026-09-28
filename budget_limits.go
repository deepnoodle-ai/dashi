package kernel

import (
	"fmt"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
	"github.com/deepnoodle-ai/kernel/tool"
)

func checkSendBudget(
	s *replayState,
	request llm.Request,
	model string,
) (*llm.Cost, *Money, *Stop) {
	snapshot := request.Snapshot()
	var atRisk *llm.Cost
	if snapshot.ModelInfo != nil && snapshot.ModelInfo.Pricing != nil {
		if value, ok := llm.MaxCostAtRisk(
			request,
			*snapshot.ModelInfo.Pricing,
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
		} else if hasHostedTools(request) {
			reason = fmt.Sprintf(
				"cost risk unavailable for model %s: provider-hosted tool "+
					"request counts are unbounded; remove hosted tools to "+
					"enforce the %s %s cost cap",
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
		fit := llm.FitOutputTokens(
			request,
			*snapshot.ModelInfo.Pricing,
			remaining,
		)
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
				Kind:       LimitCost,
				CostLimit:  cap.Amount,
				Currency:   cap.Currency,
				CostUsed:   used,
				CostAtRisk: atRisk.Total,
				FitTokens:  fit,
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

func hasHostedTools(request llm.Request) bool {
	for _, def := range request.Input().Tools {
		if def.Kind == tool.KindProviderHosted {
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
	b := s.Budget
	output := s.Spend.Usage.OutputTokens
	if b.MaxOutputTokens > 0 &&
		(output > b.MaxOutputTokens ||
			!overOnly && output == b.MaxOutputTokens) {
		return &Stop{
			Kind:   StopLimited,
			Reason: "output_tokens",
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
			Reason: "total_tokens",
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
			Kind: StopLimited,
			Reason: "cost cannot be checked: an earlier charge is unknown " +
				"or in another currency",
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
			Reason: "cost",
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
