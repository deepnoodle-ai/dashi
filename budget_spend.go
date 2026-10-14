package dashi

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

// String returns a human-readable cost summary with exact amounts and currencies
// in lexical order. Model and local tool costs remain separate. With no recorded
// amount, it returns "no recorded cost", followed by any partial or unknown
// cost counters.
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

// Spend is rebuilt from the recorded results of model and tool calls.
// MaxCost applies to Cost, while ToolCost is reported separately for the
// host's own spending checks.
// CallsWithoutFinalUsage counts finished calls whose provider never
// reported final usage; Usage and Cost undercount them. A call still
// running, or lost with its worker and not yet recovered, is not counted.
// UsageIncomplete marks failed usage aggregation, independently of unknown
// money amounts. Diagnostic counters saturate at math.MaxInt rather
// than wrapping; saturated counts are lower bounds.
type Spend struct {
	// Usage aggregates disjoint token buckets across model calls.
	Usage llm.Usage `json:"usage"`
	// UsageIncomplete means derivation or aggregation failed, so Usage is
	// incomplete. Retained counts cannot enforce a token cap. It does not
	// mark money unknown or missing provider reports (see
	// CallsWithoutFinalUsage).
	UsageIncomplete bool `json:"usage_incomplete,omitempty"`
	// Cost contains exact model-call charges by currency, without
	// conversion. It is a lower bound when CostPartial or CostUnknown is
	// nonzero: unpriced requests and unknown charges are not in it.
	Cost map[string]money.Amount `json:"cost,omitempty"`
	// ToolCost contains exact local tool charges outside MaxCost.
	ToolCost map[string]money.Amount `json:"tool_cost,omitempty"`
	// CallsWithoutFinalUsage counts sent calls cut off before final usage
	// (UsageSourceCutOff) or with no reported usage (UsageSourceMissing).
	// Usage and Cost count only what was reported for them, so both
	// undercount these calls.
	CallsWithoutFinalUsage int `json:"calls_without_final_usage,omitempty"`
	// CostUnknown counts unknown or cap-incompatible charges and monetary
	// aggregation failures in either category. Old serialized values can
	// also include unidentified usage failures and remain conservative.
	CostUnknown int `json:"cost_unknown,omitempty"`
	// CostPartial counts calls whose known cost is in Cost but whose provider
	// tool requests were partly unpriced.
	CostPartial int `json:"cost_partial,omitempty"`
	// UnpricedRequests sums, by meter key, the provider tool requests of
	// partial costs that had no price. Counts saturate at math.MaxInt and
	// then give a lower bound, while CostPartial retains the missing-price
	// evidence.
	UnpricedRequests map[string]int `json:"unpriced_requests,omitempty"`
}

// Clone copies the mutable usage and currency totals for independent use.
func (s Spend) Clone() Spend {
	s.Usage = *s.Usage.Clone()
	s.Cost = maps.Clone(s.Cost)
	s.ToolCost = maps.Clone(s.ToolCost)
	s.UnpricedRequests = maps.Clone(s.UnpricedRequests)
	return s
}

// Add returns the sum of s and o for reports that cover several turns, such
// as a parent and its children. Model Cost and ToolCost stay in separate
// maps, and every counter keeps its own meaning, so a total never hides
// which calls lacked final usage, or which costs were unpriced or unknown. Failed usage addition
// retains the prior Usage and sets UsageIncomplete. Monetary overflow keeps
// the prior currency subtotal and adds one to CostUnknown. Later additions
// can still fit, so retained subtotals depend on addition order after failure.
func (s Spend) Add(o Spend) Spend {
	sum := s.Clone()
	o = o.Clone()
	sum.UsageIncomplete = sum.UsageIncomplete || o.UsageIncomplete
	sum.addUsage(&o.Usage)
	sum.Cost = addCosts(&sum, sum.Cost, o.Cost)
	sum.ToolCost = addCosts(&sum, sum.ToolCost, o.ToolCost)
	sum.CallsWithoutFinalUsage = cappedCount(
		sum.CallsWithoutFinalUsage,
		o.CallsWithoutFinalUsage,
	)
	sum.CostUnknown = cappedCount(sum.CostUnknown, o.CostUnknown)
	sum.CostPartial = cappedCount(sum.CostPartial, o.CostPartial)
	for meter, n := range o.UnpricedRequests {
		if sum.UnpricedRequests == nil {
			sum.UnpricedRequests = map[string]int{}
		}
		sum.UnpricedRequests[meter] = cappedCount(sum.UnpricedRequests[meter], n)
	}
	return sum
}

func addCosts(
	sum *Spend,
	into, from map[string]money.Amount,
) map[string]money.Amount {
	for currency, amount := range from {
		into = addSpendCost(sum, into, currency, amount)
	}
	return into
}

// UsageSource says where a model call's counted usage came from.
type UsageSource string

const (
	// UsageSourceReported means the provider reported complete, final usage for
	// a call that finished.
	UsageSourceReported UsageSource = "reported"
	// UsageSourceCutOff means the provider reported some usage, but the call
	// did not finish normally: it was interrupted, its result is unknown, or
	// it has no valid stop. The usage is a lower bound.
	UsageSourceCutOff UsageSource = "cut_off"
	// UsageSourceMissing means no usage was reported for a call that may have
	// run: it is still running, its worker stopped, or it was cut before
	// usage arrived. It counts zero. A result the host supplies later can
	// replace it.
	UsageSourceMissing UsageSource = "missing"
	// UsageSourceNotSent means the provider verifiably never ran the request
	// (not dispatched, or rejected before execution), so it costs nothing.
	// Unlike UsageSourceMissing, nothing is unreported.
	UsageSourceNotSent UsageSource = "not_sent"
)

// ModelCallCost is what one model call counts toward Spend. Spend is the
// sum of a turn's model calls plus local tool costs, so a host that charges
// each call (rounding each, or releasing a hold by model call ID) reads these
// rather than re-deriving them from model facts.
type ModelCallCost struct {
	ModelCallID string       `json:"model_call_id"`
	Purpose     ModelPurpose `json:"purpose,omitempty"`
	Provider    string       `json:"provider,omitempty"`
	Model       string       `json:"model,omitempty"`
	// Finished means a result or failure is recorded. An unfinished call
	// is still running; it counts nothing until it finishes.
	Finished    bool        `json:"finished,omitempty"`
	UsageSource UsageSource `json:"usage_source"`
	// Usage is the usage counted into Spend.Usage: what the provider
	// reported, nil when it reported none, the call was not sent, or usage
	// could not be derived.
	Usage *llm.Usage `json:"usage,omitempty"`
	// Cost is the amount counted into Spend.Cost: the provider's reported
	// cost, else the price sheet applied to Usage. A priced call with no
	// reported usage has a zero cost. Nil means zero for a call that was
	// never sent, and otherwise an unknown cost (an unpriced model, for
	// example), counted in Spend.CostUnknown unless no usage was reported.
	// A partial cost (Cost.Partial), or the cost of a call that is cut off
	// or missing usage, is a lower bound.
	Cost *llm.Cost `json:"cost,omitempty"`
	// WorstCaseCost is the worst case computed before the call was sent, nil when
	// the call had no cost bound.
	WorstCaseCost *llm.Cost `json:"worst_case_cost,omitempty"`
	// Exact means the cost is exact: a complete cost from final reported
	// usage, or zero for a call that never ran. A cost that is not exact is
	// a lower bound or unknown.
	Exact bool `json:"exact,omitempty"`
}

// ModelCallCostOf classifies one model call from the fact recorded when it
// started and the fact recorded when it finished, or nil while the call is
// pending. It derives the charge from the facts every time; no charge is
// stored on them. It is the same derivation
// TurnView.ModelCallCosts uses. A recorder that reserved credits on the
// StepModelCallStarted step can call it in the commit of the StepModelCallFinished or
// StepModelCallFinishedByHost step, with the start fact it stored when it
// reserved, to charge or release the hold in the same transaction. A nil
// start fact gives the zero value.
func ModelCallCostOf(callStart, finish *ModelFact) ModelCallCost {
	if callStart == nil {
		return ModelCallCost{}
	}
	a, _ := modelCallCostOf(callStart, finish)
	return a
}

// modelCallCostOf is ModelCallCostOf that also reports whether usage
// derivation was incomplete, which sets Spend.UsageIncomplete.
func modelCallCostOf(callStart, finish *ModelFact) (ModelCallCost, bool) {
	a := ModelCallCost{
		ModelCallID: callStart.ModelCallID,
		Purpose:     callStart.Purpose,
		Provider:    callStart.Provider,
		Model:       callStart.Model,
		Finished:    finish != nil,
	}
	if callStart.WorstCaseCost != nil {
		a.WorstCaseCost = new(callStart.WorstCaseCost.Clone())
	}
	usage, charge, source, incomplete := modelCallCost(callStart, finish)
	a.UsageSource = source
	if source == UsageSourceNotSent {
		a.Exact = true
		return a, false
	}
	a.Usage = usage
	if charge != nil {
		a.Cost = new(charge.Clone())
	}
	a.Exact = a.UsageSource == UsageSourceReported && a.Cost != nil && !a.Cost.Partial()
	return a, incomplete
}

func (s *Spend) addUsage(usage *llm.Usage) {
	if err := s.Usage.Add(usage); err != nil {
		s.UsageIncomplete = true
	}
}

func addSpendCost(
	sum *Spend,
	into map[string]money.Amount,
	currency string,
	amount money.Amount,
) map[string]money.Amount {
	if into == nil {
		into = map[string]money.Amount{}
	}
	if err := addCurrencyCost(into, currency, amount); err != nil {
		sum.CostUnknown = cappedCount(sum.CostUnknown, 1)
	}
	return into
}

func (s Spend) costIn(currency string) (money.Amount, bool) {
	used := s.Cost[currency]
	if s.CostUnknown > 0 {
		return used, false
	}
	for unit, amount := range s.Cost {
		if unit != currency && amount != 0 {
			return used, false
		}
	}
	return used, true
}

func addCurrencyCost(
	totals map[string]money.Amount,
	currency string,
	amount money.Amount,
) error {
	total, ok := totals[currency].Add(amount)
	if !ok {
		return errors.New("dashi: cumulative cost overflows")
	}
	totals[currency] = total
	return nil
}
