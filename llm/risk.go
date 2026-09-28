package llm

import (
	"math"
	"math/big"

	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
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
// call count also cannot be bounded before dispatch. It also returns false
// when the bound overflows the nano range, so an admission check treats that
// call as unbounded rather than cheap.
//
// A risk bound rounds up to the nano: Input and Output each round up, and
// Total rounds up the exact sum of both products once, so Total never exceeds
// Input+Output and rounding can never let a cap be breached.
func MaxCostAtRisk(request Request, pricing Pricing) (Cost, bool) {
	bound, ok := riskBound(request, pricing)
	if !ok {
		return Cost{}, false
	}
	var input, output, total exactSum
	input.add(bound.input, bound.inputRate)
	output.add(bound.maxOutput, bound.outputRate)
	total.addSum(&input)
	total.addSum(&output)
	cost := Cost{
		Currency: pricing.Currency,
		Source:   CostSourceListPriceEstimate,
	}
	var inputOK, outputOK, totalOK bool
	cost.Input, inputOK = input.round(roundUp)
	cost.Output, outputOK = output.round(roundUp)
	cost.Total, totalOK = total.round(roundUp)
	if !inputOK || !outputOK || !totalOK {
		return Cost{}, false
	}
	return cost, cost.Validate() == nil
}

// FitOutputTokens returns the largest output allowance whose MaxCostAtRisk
// total would fit in the given remaining amount. It uses exact arithmetic, so
// the returned allowance fits even at the nano boundary.
func FitOutputTokens(
	request Request,
	pricing Pricing,
	remaining money.Amount,
) int {
	bound, ok := riskBound(request, pricing)
	if !ok || remaining < 0 {
		return 0
	}
	// A rounded-up total fits when the exact product sum is at most
	// remaining × tokensPerRate.
	var available, spent exactSum
	available.add(tokensPerRate, remaining)
	spent.add(bound.input, bound.inputRate)
	room := new(big.Int).Sub(&available.value, &spent.value)
	if room.Sign() < 0 {
		return 0
	}
	if bound.outputRate == 0 {
		return bound.maxOutput
	}
	room.Quo(room, big.NewInt(bound.outputRate.Nanos()))
	if !room.IsInt64() || room.Int64() >= int64(bound.maxOutput) {
		return bound.maxOutput
	}
	return int(room.Int64())
}

// risk is the token counts and highest rates that bound a call's cost.
type risk struct {
	input      int
	maxOutput  int
	inputRate  money.Amount
	outputRate money.Amount
}

func riskBound(request Request, pricing Pricing) (risk, bool) {
	input := EstimatedInputTokens(request)
	maxOutput := request.Effective().MaxTokens
	if maxOutput <= 0 || pricing.Validate() != nil {
		return risk{}, false
	}
	for _, def := range request.Input().Tools {
		if def.Kind == tool.KindProviderHosted {
			return risk{}, false
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
	return risk{
		input:      input,
		maxOutput:  maxOutput,
		inputRate:  inputRate,
		outputRate: outputRate,
	}, true
}
