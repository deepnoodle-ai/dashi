package llm

import (
	"math"

	"github.com/deepnoodle-ai/kernel/tool"
)

// EstimatedInputTokens is a conservative size bound for a prepared request.
// The wire body is the closest available representation before dispatch.
func EstimatedInputTokens(request Request) int {
	size := len(request.Body())
	if size > math.MaxInt-1024 {
		return math.MaxInt
	}
	return size + 1024
}

// MaxCostAtRisk estimates input at the highest input-side rate and the full
// effective output allowance. It returns false when the call is unpriced or
// its output allowance is unknown. A priced hosted operation without a known
// call count also cannot be bounded before dispatch.
func MaxCostAtRisk(request Request, pricing Pricing) (Cost, bool) {
	input := EstimatedInputTokens(request)
	maxOutput := request.Effective().MaxTokens
	if maxOutput <= 0 || pricing.Validate() != nil {
		return Cost{}, false
	}
	for _, def := range request.Input().Tools {
		if def.Kind == tool.KindProviderHosted {
			return Cost{}, false
		}
	}
	rates := pricing.Base
	for _, tier := range pricing.Tiers {
		if input >= tier.MinInputTokens {
			rates = tier.Rates
		}
	}
	inputRate := max(rates.Input, rates.CacheRead, rates.CacheWrite)
	outputRate := rates.Output
	for _, modality := range pricing.Modality {
		inputRate = max(
			inputRate,
			modality.Input,
			modality.CacheRead,
			modality.CacheWrite,
		)
		outputRate = max(outputRate, modality.Output)
	}
	cost := Cost{
		Input:    float64(input) * inputRate / 1_000_000,
		Output:   float64(maxOutput) * outputRate / 1_000_000,
		Currency: pricing.Currency,
		Source:   CostSourceListPriceEstimate,
	}
	cost.Total = cost.Input + cost.Output
	return cost, cost.Validate() == nil
}

// FitOutputTokens returns the largest output allowance that could fit in the
// given remaining amount, using the same bound as MaxCostAtRisk.
func FitOutputTokens(request Request, pricing Pricing, remaining float64) int {
	risk, ok := MaxCostAtRisk(request, pricing)
	if !ok || remaining < risk.Input {
		return 0
	}
	maxOutput := request.Effective().MaxTokens
	if risk.Output == 0 {
		return maxOutput
	}
	rate := risk.Output / float64(maxOutput)
	possible := math.Floor((remaining - risk.Input) / rate)
	if possible >= float64(maxOutput) {
		return maxOutput
	}
	fit := int(possible)
	return max(0, min(maxOutput, fit))
}
