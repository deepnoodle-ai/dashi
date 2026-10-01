package dashi

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/money"
)

// String returns a human-readable cost summary with exact amounts and currencies
// in lexical order. Model and local tool costs remain separate. With no recorded
// amount, it returns "no recorded cost", followed by any uncertainty counters.
//
// Positive CostPartial and CostUnknown counters appear separately. Either marks
// model amounts as "at least". CostUnknown also marks tool amounts because it
// does not identify the affected category. Old values can include unidentified
// usage failures. UsageIncomplete appends "usage incomplete" without qualifying
// money. Usage counts and unpriced meter details are omitted. Use Spend fields
// or JSON for machine processing.
func (s Spend) String() string {
	var sections []string
	if len(s.Cost) > 0 {
		sections = append(sections, "model: "+formatSpendCosts(s.Cost,
			s.CostPartial > 0 || s.CostUnknown > 0))
	}
	if len(s.ToolCost) > 0 {
		sections = append(sections, "tools: "+formatSpendCosts(s.ToolCost,
			s.CostUnknown > 0))
	}
	if len(sections) == 0 {
		sections = append(sections, "no recorded cost")
	}
	if s.CostPartial > 0 {
		sections = append(sections, "partial: "+strconv.Itoa(s.CostPartial))
	}
	if s.CostUnknown > 0 {
		sections = append(sections, "unknown: "+strconv.Itoa(s.CostUnknown))
	}
	if s.UsageIncomplete {
		sections = append(sections, "usage incomplete")
	}
	return strings.Join(sections, "; ")
}

func formatSpendCosts(costs map[string]money.Amount, lowerBound bool) string {
	amounts := make([]string, 0, len(costs))
	for _, currency := range slices.Sorted(maps.Keys(costs)) {
		amounts = append(amounts, costs[currency].String()+" "+currency)
	}
	text := strings.Join(amounts, ", ")
	if lowerBound {
		return "at least " + text
	}
	return text
}
