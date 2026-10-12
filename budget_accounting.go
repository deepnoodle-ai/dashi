package dashi

import (
	"math"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// modelCallCost derives what one call counts toward Spend from its facts
// (decision 47): only the usage its provider reported, never a filled-in
// token count. It returns that usage, its cost (nil when unknown), where the
// usage came from, and whether derivation was incomplete. A call with no
// reported usage counts zero, at a known zero cost when the model is priced.
func modelCallCost(
	callStart *ModelFact,
	finish *ModelFact,
) (*llm.Usage, *llm.Cost, UsageSource, bool) {
	if finish != nil && (finish.SendStatus == llm.SendStatusProviderRejected ||
		finish.SendStatus == llm.SendStatusNotSent || finish.NotSent) {
		return nil, nil, UsageSourceNotSent, false
	}
	var usage *llm.Usage
	var reported *llm.Cost
	if finish != nil && finish.Result != nil {
		if finish.Result.Usage != nil {
			usage = finish.Result.Usage.Copy()
		}
		reported = finish.Result.Cost
	}
	source := UsageSourceMissing
	if usage != nil {
		source = UsageSourceReported
		if modelUsageMayBePartial(finish) {
			source = UsageSourceCutOff
		}
	}
	incomplete := false
	if finish != nil && finish.Result != nil &&
		(usage == nil || !usage.RequestsComplete) {
		next := &llm.Usage{}
		if usage != nil {
			next = usage.Copy()
		}
		// Enrichment of valid recorded facts can overflow. Preserve the
		// original usage atomically, and never price its omitted
		// contribution.
		if !addUnreportedRequests(next, callStart, finish.Result) ||
			next.Validate() != nil {
			incomplete = true
		} else if usage != nil || len(next.Requests) > 0 {
			usage = next
		}
	} else if usage != nil && usage.Validate() != nil {
		incomplete = true
	}
	if reported != nil {
		return usage, reported, source, incomplete
	}
	if !incomplete && callStart.ModelInfo != nil && callStart.ModelInfo.Pricing != nil {
		priced := usage
		if priced == nil {
			priced = &llm.Usage{}
		}
		if charge, ok := callStart.ModelInfo.Pricing.Estimate(priced); ok {
			return usage, &charge, source, false
		}
	}
	return usage, nil, source, incomplete
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
