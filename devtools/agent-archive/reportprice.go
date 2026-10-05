package main

import (
	"strings"

	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// The report prices every usage row with Dashi's own catalog through
// llm.Pricing.Estimate; it adds no rates of its own. See
// design/agent-archive-report.md.

// Why a model has no price.
const (
	unpricedNotInCatalog = "not in catalog"
	unpricedNoPrice      = "no price in catalog"
	unpricedNoEstimate   = "no estimate" // Estimate refused the usage
)

// speedFast marks a usage row made in fast mode, which the catalog has no
// price for yet; it is priced at standard rates as a lower bound.
const speedFast = "fast"

// providerOf is the catalog provider of an app's model calls.
func providerOf(app string) string {
	if app == appCodex {
		return "openai"
	}
	return "anthropic"
}

// modelPrice is what the catalog knows about one model's price.
type modelPrice struct {
	Pricing  *llm.Pricing // nil when unpriced
	Unpriced string       // why Pricing is nil
}

// priceBook looks models up in the catalog once each.
type priceBook map[string]modelPrice

// lookup finds a model by its exact ID, then as the dated snapshot an
// entry serves (claude-haiku-4-5-20251001).
func (b priceBook) lookup(app, model string) modelPrice {
	provider := providerOf(app)
	key := provider + "\x00" + model
	if p, ok := b[key]; ok {
		return p
	}
	entry, ok := catalog.Find(provider, model)
	if !ok {
		for _, e := range catalog.Entries(provider) {
			if model != "" && e.Serves(model) {
				entry, ok = e, true
				break
			}
		}
	}
	var p modelPrice
	switch {
	case !ok:
		p.Unpriced = unpricedNotInCatalog
	case entry.Pricing == nil:
		p.Unpriced = unpricedNoPrice
	default:
		p.Pricing = entry.Pricing
	}
	b[key] = p
	return p
}

// tokenCounts are the token columns of a usage or app_usage row.
// CacheWrite includes CacheWrite1h; Output includes Reasoning.
type tokenCounts struct {
	Input, CacheRead, CacheWrite, CacheWrite1h, Output, Reasoning, WebSearch int
}

func (t *tokenCounts) add(u tokenCounts) {
	t.Input += u.Input
	t.CacheRead += u.CacheRead
	t.CacheWrite += u.CacheWrite
	t.CacheWrite1h += u.CacheWrite1h
	t.Output += u.Output
	t.Reasoning += u.Reasoning
	t.WebSearch += u.WebSearch
}

// minus returns t less u, each column clamped at zero. The one-hour split
// is dropped: app_usage has none.
func (t tokenCounts) minus(u tokenCounts) tokenCounts {
	sub := func(a, b int) int { return max(a-b, 0) }
	return tokenCounts{
		Input:      sub(t.Input, u.Input),
		CacheRead:  sub(t.CacheRead, u.CacheRead),
		CacheWrite: sub(t.CacheWrite, u.CacheWrite),
		Output:     sub(t.Output, u.Output),
		Reasoning:  sub(t.Reasoning, u.Reasoning),
		WebSearch:  sub(t.WebSearch, u.WebSearch),
	}
}

// usage is t as llm.Usage, its cache writes as recorded.
func (t tokenCounts) usage() llm.Usage {
	u := llm.Usage{
		InputTokens:                t.Input,
		CacheReadInputTokens:       t.CacheRead,
		CacheCreationInputTokens:   t.CacheWrite,
		CacheCreation1hInputTokens: t.CacheWrite1h,
		OutputTokens:               t.Output,
		ReasoningTokens:            t.Reasoning,
	}
	if t.WebSearch > 0 {
		u.Requests = map[string]int{llm.MeterWebSearch: t.WebSearch}
	}
	return u
}

// amount is the list price of some model calls. Low and High are equal
// unless a high estimate applies: High is what tables show.
type amount struct {
	Low, High money.Amount
	// LowerBound means some part could not be priced: an unpriced model,
	// a fast call, or an unpriced provider tool request.
	LowerBound    bool
	UnpricedCalls int
	// Rows counts what was added, so an empty cell differs from $0.
	Rows int
}

func (a *amount) add(b amount) {
	a.Low += b.Low
	a.High += b.High
	a.LowerBound = a.LowerBound || b.LowerBound
	a.UnpricedCalls += b.UnpricedCalls
	a.Rows += b.Rows
}

// isRange reports whether a high estimate applies.
func (a amount) isRange() bool { return a.Low != a.High }

// estimate prices one usage, reporting false when Estimate refuses it.
func estimate(p *llm.Pricing, u llm.Usage) (llm.Cost, bool) {
	return p.Estimate(&u)
}

// priceCall prices one usage row (decision 2). Codex never reports cache
// writes, so its uncached input is priced twice: all as cache writes for
// High (a high estimate) and all as input for Low (decision 3).
func priceCall(p modelPrice, app string, t tokenCounts, speed string) (amount, string) {
	a := amount{Rows: 1, LowerBound: speed == speedFast}
	if p.Pricing == nil {
		a.LowerBound, a.UnpricedCalls = true, 1
		return a, p.Unpriced
	}
	low := t.usage()
	high := low
	if app == appCodex {
		high.InputTokens, high.CacheCreationInputTokens = 0, t.Input
	}
	lowCost, ok1 := estimate(p.Pricing, low)
	highCost, ok2 := estimate(p.Pricing, high)
	if !ok1 || !ok2 {
		a.LowerBound, a.UnpricedCalls = true, 1
		return a, unpricedNoEstimate
	}
	a.Low, a.High = lowCost.Total, highCost.Total
	a.LowerBound = a.LowerBound || lowCost.Partial() || highCost.Partial()
	return a, ""
}

// priceNotInSessionFiles prices a session's calls not in session files
// for one model as one usage (decision 7). app_usage has no one-hour
// split, so the cache writes are priced all at the 5-minute rate (Low)
// and all at the one-hour rate (High).
func priceNotInSessionFiles(p modelPrice, t tokenCounts) (amount, string) {
	a := amount{Rows: 1}
	if p.Pricing == nil {
		a.LowerBound = true
		return a, p.Unpriced
	}
	low := t.usage()
	low.CacheCreation1hInputTokens = 0
	high := low
	high.CacheCreation1hInputTokens = high.CacheCreationInputTokens
	lowCost, ok1 := estimate(p.Pricing, low)
	highCost, ok2 := estimate(p.Pricing, high)
	if !ok1 || !ok2 {
		a.LowerBound = true
		return a, unpricedNoEstimate
	}
	a.Low, a.High = lowCost.Total, highCost.Total
	a.LowerBound = lowCost.Partial() || highCost.Partial()
	return a, ""
}

// appUsageModel is an app_usage model name as usage rows name it: without
// Claude Code's "[1m]" suffix (Anthropic's current models have one price
// at any length).
func appUsageModel(model string) string {
	return strings.TrimSuffix(model, "[1m]")
}
