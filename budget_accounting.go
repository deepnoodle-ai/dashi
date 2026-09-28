package kernel

import (
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/llm"
)

func attemptSpend(
	intent *ModelFact,
	settlement *ModelFact,
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

func setSettlementCharge(intent *ModelFact, settlement *ModelFact) {
	if intent == nil || settlement == nil {
		return
	}
	_, charge, estimated, counted := attemptSpend(intent, settlement)
	if counted {
		settlement.Charge = charge
		settlement.EstimatedUsage = estimated
	}
}
