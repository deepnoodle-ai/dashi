package dashi

import (
	"math"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func modelCallCost(
	callStart *ModelFact,
	finish *ModelFact,
) (*llm.Usage, *llm.Cost, bool, bool, bool) {
	if finish != nil && (finish.SendStatus == llm.SendStatusProviderRejected ||
		finish.SendStatus == llm.SendStatusNotSent || finish.NotSent) {
		return nil, nil, false, false, false
	}
	var usage *llm.Usage
	var reported *llm.Cost
	if finish != nil && finish.Result != nil {
		usage = finish.Result.Usage
		reported = finish.Result.Cost
	}
	hasReportedUsage := usage != nil
	estimated := !hasReportedUsage
	if estimated {
		usage = &llm.Usage{InputTokens: callStart.EstimatedInputTokens}
	} else {
		usage = usage.Copy()
	}
	next := usage.Copy()
	if estimated && finish != nil && finish.Result != nil {
		next.OutputTokens = cappedOutputEstimate(callStart, finish.Result)
	}
	if modelUsageMayBePartial(finish) {
		// Streamed usage may describe only the input or an earlier output
		// frame. Keep reported counts and include later visible output only
		// when the model result is incomplete or its outcome is unknown.
		estimated = true
		if next.TotalInputTokens() == 0 {
			next.InputTokens = callStart.EstimatedInputTokens
		}
		if finish.Result != nil {
			next.OutputTokens = max(
				next.OutputTokens,
				cappedOutputEstimate(callStart, finish.Result),
			)
		}
	}
	incomplete := false
	if finish != nil && !next.RequestsComplete {
		incomplete = !addUnreportedRequests(next, callStart, finish.Result)
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
	if finish != nil && finish.Cost != nil {
		return usage, finish.Cost, estimated, true, incomplete
	}
	if reported != nil {
		return usage, reported, estimated, true, incomplete
	}
	if !incomplete && callStart.ModelInfo != nil && callStart.ModelInfo.Pricing != nil {
		if charge, ok := callStart.ModelInfo.Pricing.Estimate(usage); ok {
			return usage, &charge, estimated, true, false
		}
	}
	return usage, nil, estimated, true, incomplete
}

func modelUsageMayBePartial(finish *ModelFact) bool {
	if finish == nil {
		return false
	}
	if finish.Interrupted != InterruptNone || finish.ResultUnknown {
		return true
	}
	if finish.Result == nil {
		return false
	}
	// A validation error can follow a finished response with final usage.
	// Missing or invalid stops, though, give no evidence that it ended.
	switch finish.Result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit,
		llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopOther:
		return false
	default:
		return true
	}
}

// addUnreportedRequests covers provider tool work whose billing the provider
// never reported: the call's usage is missing or not final
// (Usage.RequestsComplete), as for a stream cut off before its final usage. Counts an adapter inferred
// from the blocks seen so far are kept, but they may miss billed work, so
// the call gets at least one llm.MeterUnreported request, which no sheet
// prices, and its cost shows as partial rather than complete. Visible uses
// beyond the counted requests are all unreported. Uses with an error result
// are skipped, since providers do not bill refused or failed uses.
//
// Some provider tool work leaves no block at all: Gemini reports search
// grounding only in metadata near the end of a stream. So a request that
// declared a provider tool counts as provider tool use even with no visible
// use.
func addUnreportedRequests(
	usage *llm.Usage,
	callStart *ModelFact,
	result *llm.Result,
) bool {
	uses := 0
	if result != nil {
		failed := map[string]bool{}
		for _, part := range result.Message.Content {
			if part.ProviderToolResult != nil && part.ProviderToolResult.IsError {
				failed[part.ProviderToolResult.CallID] = true
			}
		}
		for _, part := range result.Message.Content {
			use := part.ProviderToolUse
			if part.Kind == llm.ContentProviderToolUse && use != nil &&
				!use.IsError && !failed[use.ID] {
				uses++
			}
		}
	}
	counted := 0
	for _, count := range usage.Requests {
		counted = cappedCount(counted, max(count, 0))
	}
	if uses == 0 && counted == 0 && !declaresProviderTool(callStart) {
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

func declaresProviderTool(callStart *ModelFact) bool {
	if callStart == nil {
		return false
	}
	for _, def := range callStart.Tools {
		if def.Kind == tool.KindProviderTool {
			return true
		}
	}
	return false
}

func estimatedOutputTokens(result *llm.Result) int {
	runes := utf8.RuneCountInString(result.Message.Text())
	return runes/4 + min(1, runes%4)
}

// cappedOutputEstimate estimates a finished call's output from its text,
// capped at the effective max_tokens recorded when the call started, as the
// live run's estimate is, since a provider bills no more output than the
// call allowed. Rebuilding the view from the record then never counts more
// than the run did.
func cappedOutputEstimate(callStart *ModelFact, result *llm.Result) int {
	estimate := estimatedOutputTokens(result)
	if limit := callStart.Effective.MaxTokens; limit > 0 {
		estimate = min(estimate, limit)
	}
	return estimate
}

func setFinishCharge(callStart *ModelFact, finish *ModelFact) {
	if callStart == nil || finish == nil {
		return
	}
	_, charge, estimated, counted, _ := modelCallCost(callStart, finish)
	if counted {
		finish.Cost = charge
		finish.EstimatedUsage = estimated
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
