package llm

import (
	"maps"
	"math"
	"math/big"
	"slices"
	"time"

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

// WorstCaseCost estimates input at the highest input-side rate and the full
// effective output allowance. It returns false when the call is unpriced or
// its output allowance is unknown. A priced hosted operation without a known
// call count also cannot be bounded before dispatch. It also returns false
// when the bound overflows the nano range, so an admission check treats that
// call as unbounded rather than cheap.
//
// A call that may write one-hour cache entries (see writesOneHour) includes
// Rates.CacheWrite1h among the input rates; without that rate it has no
// bound.
//
// A risk bound rounds up to the nano: Input and Output each round up, and
// Total rounds up the exact sum of both products once, so Total never exceeds
// Input+Output and rounding can never let a cap be breached.
func WorstCaseCost(request Request, pricing Pricing) (Cost, bool) {
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

// CanBoundCost reports whether WorstCaseCost can bound, in currency, a call
// to this model that uses default settings under CapabilityAdjust (the
// default policy). It answers a readiness check from model knowledge alone,
// without preparing a request. It is true when the model has a valid price
// sheet in exactly currency and accepts an output limit, or does not say:
// preparation then sends DefaultMaxTokens, lowered to a known
// MaxOutputTokens, so the output side is finite, and the input side is
// bounded by the request body.
//
// True says only that ordinary calls are bounded. A specific call can still
// have no bound:
//   - it offers a provider-hosted tool, whose call count is unknown before
//     dispatch;
//   - its prompt cache lifetime is above five minutes and a rate set it may
//     use lacks Rates.CacheWrite1h;
//   - the client uses CapabilityStrict and the model does not say it
//     accepts an output limit, so preparation sends no default allowance.
//
// A bound is a worst case at list prices, not a quote: the call's spend is
// priced from usage once it settles.
func (m ModelInfo) CanBoundCost(currency string) bool {
	if m.Pricing == nil ||
		m.Pricing.Currency != currency ||
		m.Pricing.Validate() != nil {
		return false
	}
	return m.Capabilities.acceptsDefaultMaxTokens(CapabilityAdjust)
}

// FitOutputTokens returns the largest output allowance whose WorstCaseCost
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
		if def.Kind == tool.KindProviderTool {
			return risk{}, false
		}
	}
	rates := pricing.Base
	for _, tier := range pricing.Tiers {
		if input >= tier.MinInputTokens {
			rates = tier.Rates
		}
	}
	oneHour := writesOneHour(request)
	inputRate := max(rates.Input, rates.CacheRead, rates.CacheWrite)
	outputRate := rates.Output
	sets := append([]Rates{rates}, slices.Collect(maps.Values(pricing.Modality))...)
	for _, set := range sets {
		inputRate = max(inputRate, set.Input, set.CacheRead, set.CacheWrite)
		outputRate = max(outputRate, set.Output)
		if oneHour {
			if set.CacheWrite1h == nil {
				return risk{}, false
			}
			inputRate = max(inputRate, *set.CacheWrite1h)
		}
	}
	return risk{
		input:      input,
		maxOutput:  maxOutput,
		inputRate:  inputRate,
		outputRate: outputRate,
	}, true
}

// writesOneHour reports whether a prepared call may write one-hour cache
// entries: its effective lifetime is above the 5-minute default and the
// model's known lifetimes do not show that the route sends another one, as
// OpenAI's 24 hours or 30 minutes. With no lifetimes known it assumes one
// hour, so a bound never falls below the charge.
func writesOneHour(request Request) bool {
	snapshot := request.Snapshot()
	ttl := snapshot.Effective.PromptCache.TTL
	if ttl <= 5*time.Minute {
		return false
	}
	var offered []time.Duration
	if info := snapshot.ModelInfo; info != nil {
		offered = info.Capabilities.CacheTTLs
	}
	return len(offered) == 0 || CacheLifetime(ttl, offered) == time.Hour
}
