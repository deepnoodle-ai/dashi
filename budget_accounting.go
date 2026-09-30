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
) (*llm.Usage, *llm.Cost, bool, bool, bool) {
	if settlement != nil && (settlement.Failure == llm.FailureRejected ||
		settlement.Failure == llm.FailureNotDispatched || settlement.NoDispatch) {
		return nil, nil, false, false, false
	}
	var usage *llm.Usage
	var reported *llm.Cost
	if settlement != nil && settlement.Result != nil {
		usage = settlement.Result.Usage
		reported = settlement.Result.Cost
	}
	hasReportedUsage := usage != nil
	estimated := !hasReportedUsage
	if estimated {
		usage = &llm.Usage{InputTokens: intent.EstimatedInputTokens}
	} else {
		usage = usage.Copy()
	}
	next := usage.Copy()
	if estimated && settlement != nil && settlement.Result != nil {
		next.OutputTokens = cappedOutputEstimate(intent, settlement.Result)
	}
	if modelUsageMayBePartial(settlement) {
		// Streamed usage may describe only the input or an earlier output
		// frame. Keep reported counts and include later visible output only
		// when the model result is incomplete or uncertain.
		estimated = true
		if next.TotalInputTokens() == 0 {
			next.InputTokens = intent.EstimatedInputTokens
		}
		if settlement.Result != nil {
			next.OutputTokens = max(
				next.OutputTokens,
				cappedOutputEstimate(intent, settlement.Result),
			)
		}
	}
	incomplete := false
	if settlement != nil && !next.RequestsFinal {
		incomplete = !addUnreportedRequests(next, intent, settlement.Result)
	}
	if err := next.Validate(); err != nil {
		incomplete = true
	}
	// Enrichment of valid recorded facts can overflow too. Preserve the
	// original usage atomically, and never price its omitted contribution.
	if !incomplete {
		usage = next
	} else if !hasReportedUsage {
		usage = nil
	}
	if settlement != nil && settlement.Charge != nil {
		return usage, settlement.Charge, estimated, true, incomplete
	}
	if reported != nil {
		return usage, reported, estimated, true, incomplete
	}
	if !incomplete && intent.ModelInfo != nil && intent.ModelInfo.Pricing != nil {
		if charge, ok := intent.ModelInfo.Pricing.Estimate(usage); ok {
			return usage, &charge, estimated, true, false
		}
	}
	return usage, nil, estimated, true, incomplete
}

func modelUsageMayBePartial(settlement *ModelFact) bool {
	if settlement == nil {
		return false
	}
	if settlement.Interrupted != InterruptNone || settlement.Uncertain {
		return true
	}
	if settlement.Result == nil {
		return false
	}
	// A validation error can follow a finished response with final usage.
	// Missing or invalid stops, though, provide no terminal evidence.
	switch settlement.Result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit,
		llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopOther:
		return false
	default:
		return true
	}
}

// addUnreportedRequests covers hosted work whose billing the provider never
// reported: the call's usage is missing or not final (Usage.RequestsFinal),
// as for a stream cut off before its final usage. Counts an adapter inferred
// from the blocks seen so far are kept, but they may miss billed work, so
// the call gets at least one llm.MeterUnreported request, which no sheet
// prices, and its cost shows as partial rather than complete. Visible uses
// beyond the counted requests are all unreported. Uses with an error result
// are skipped, since providers do not bill refused or failed uses.
//
// Some hosted work leaves no block at all: Gemini reports search grounding
// only in metadata near the end of a stream. So a request that declared a
// hosted tool counts as hosted activity even with no visible use.
func addUnreportedRequests(
	usage *llm.Usage,
	intent *ModelFact,
	result *llm.Result,
) bool {
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
		counted = cappedCount(counted, max(count, 0))
	}
	if uses == 0 && counted == 0 && !declaresHostedTool(intent) {
		return true
	}
	count, ok := sumCounts(usage.Requests[llm.MeterUnreported],
		max(uses-counted, 1))
	if !ok {
		return false
	}
	if usage.Requests == nil {
		usage.Requests = map[string]int{}
	}
	usage.Requests[llm.MeterUnreported] = count
	return true
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

// cappedOutputEstimate estimates a settled call's output from its text,
// capped at the intent's effective max_tokens as the live settlement's
// estimate is, since a provider bills no more output than the call allowed.
// Replay then never counts more than the run did.
func cappedOutputEstimate(intent *ModelFact, result *llm.Result) int {
	estimate := estimatedOutputTokens(result)
	if limit := intent.Effective.MaxTokens; limit > 0 {
		estimate = min(estimate, limit)
	}
	return estimate
}

func setSettlementCharge(intent *ModelFact, settlement *ModelFact) {
	if intent == nil || settlement == nil {
		return
	}
	_, charge, estimated, counted, _ := attemptSpend(intent, settlement)
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

// cappedCount keeps diagnostic counts conservative, including malformed
// negative operands supplied through a manually constructed Spend.
func cappedCount(a, b int) int {
	if sum, ok := sumCounts(a, b); ok {
		return sum
	}
	return math.MaxInt
}
