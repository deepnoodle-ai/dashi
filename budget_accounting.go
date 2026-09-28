package dashi

import (
	"math"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
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
			guess.OutputTokens = estimatedOutputTokens(settlement.Result)
		}
		usage = guess
	} else {
		usage = usage.Copy()
	}
	if settlement != nil && settlement.Interrupted {
		// Streamed usage may describe only the input or an earlier output
		// frame. Keep reported counts and include later visible output.
		estimated = true
		if usage.TotalInputTokens() == 0 {
			usage.InputTokens = intent.EstimatedInputTokens
		}
		if settlement.Result != nil {
			usage.OutputTokens = max(
				usage.OutputTokens,
				estimatedOutputTokens(settlement.Result),
			)
		}
	}
	if estimated && settlement != nil {
		addUnreportedRequests(usage, intent, settlement.Result)
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

// addUnreportedRequests covers hosted work whose billing the provider never
// reported. Adapters count hosted requests from the final usage, so a call
// cut off before it (or one with no usage at all) can show hosted activity
// with fewer counted requests. The difference is recorded under
// llm.MeterUnreported, which no sheet prices, so the cost shows as partial
// rather than complete. Uses with an error result are skipped, since
// providers do not bill refused or failed uses.
//
// Some hosted work leaves no block at all: Gemini reports search grounding
// only in metadata near the end of a stream. So when nothing was counted or
// seen but the request declared a hosted tool, one unreported request marks
// the call as possibly incomplete.
func addUnreportedRequests(
	usage *llm.Usage,
	intent *ModelFact,
	result *llm.Result,
) {
	uses := 0
	if result != nil {
		failed := map[string]bool{}
		for _, part := range result.Message.Content {
			if part.HostedResult != nil && part.HostedResult.IsError {
				failed[part.HostedResult.CallID] = true
			}
		}
		for _, part := range result.Message.Content {
			use := part.HostedUse
			if part.Kind == llm.ContentHostedUse && use != nil &&
				!use.IsError && !failed[use.ID] {
				uses++
			}
		}
	}
	counted := 0
	for _, count := range usage.Requests {
		counted += max(count, 0)
	}
	if uses == 0 && counted == 0 && declaresHostedTool(intent) {
		uses = 1
	}
	if uses <= counted {
		return
	}
	if usage.Requests == nil {
		usage.Requests = map[string]int{}
	}
	usage.Requests[llm.MeterUnreported] += uses - counted
}

func declaresHostedTool(intent *ModelFact) bool {
	if intent == nil {
		return false
	}
	for _, def := range intent.Tools {
		if def.Kind == tool.KindProviderHosted {
			return true
		}
	}
	return false
}

func estimatedOutputTokens(result *llm.Result) int {
	runes := utf8.RuneCountInString(result.Message.Text())
	return runes/4 + min(1, runes%4)
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

// sumCounts adds two nonnegative counts, reporting overflow.
func sumCounts(a, b int) (int, bool) {
	if a < 0 || b < 0 || a > math.MaxInt-b {
		return 0, false
	}
	return a + b, true
}
