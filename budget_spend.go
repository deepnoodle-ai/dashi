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

// Spend is reconstructed from committed model and tool settlements. MaxCost
// applies to Cost, while ToolCost is reported separately for host admission.
// Estimated counts calls without final usage, including partial streamed
// frames. UsageIncomplete marks failed usage aggregation, independently of
// monetary uncertainty. Diagnostic counters saturate at math.MaxInt rather
// than wrapping; saturated counts are lower bounds.
type Spend struct {
	// Usage aggregates disjoint token buckets across attempted model calls.
	Usage llm.Usage `json:"usage"`
	// UsageIncomplete means derivation or aggregation failed, so Usage is
	// incomplete. Retained counts cannot enforce a token cap. It does not
	// mark money uncertain or missing provider reports (see Estimated).
	UsageIncomplete bool `json:"usage_incomplete,omitempty"`
	// Cost contains exact model-call charges by currency, without
	// conversion. It is a lower bound when CostPartial or CostUnknown is
	// nonzero: unpriced requests and unknown charges are not in it.
	Cost map[string]money.Amount `json:"cost,omitempty"`
	// ToolCost contains exact local tool charges outside MaxCost.
	ToolCost map[string]money.Amount `json:"tool_cost,omitempty"`
	// Estimated counts calls without complete provider-reported usage.
	Estimated int `json:"estimated,omitempty"`
	// CostUnknown counts unknown or cap-incompatible charges and monetary
	// aggregation failures in either category. Old serialized values can
	// also include unidentified usage failures and remain conservative.
	CostUnknown int `json:"cost_unknown,omitempty"`
	// CostPartial counts calls whose known charge is in Cost but whose hosted
	// requests were partly unpriced.
	CostPartial int `json:"cost_partial,omitempty"`
	// UnpricedRequests sums, by meter key, the hosted requests of partial
	// charges that had no price. Counts saturate at math.MaxInt and then give
	// a lower bound, while CostPartial retains the missing-price evidence.
	UnpricedRequests map[string]int `json:"unpriced_requests,omitempty"`
}

// Clone copies the mutable usage and currency totals for independent use.
func (s Spend) Clone() Spend {
	s.Usage = *s.Usage.Copy()
	s.Cost = maps.Clone(s.Cost)
	s.ToolCost = maps.Clone(s.ToolCost)
	s.UnpricedRequests = maps.Clone(s.UnpricedRequests)
	return s
}

// Add returns the sum of s and o for reports that cover several turns, such
// as a parent and its children. Model Cost and ToolCost stay in separate
// maps, and every counter keeps its own meaning, so a total never hides
// which part was estimated, unpriced, or unknown. Failed usage addition
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
	sum.Estimated = cappedCount(sum.Estimated, o.Estimated)
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

// SpendBasis says where an attempt's counted usage came from.
type SpendBasis string

const (
	// SpendMeasured means the provider reported complete, final usage for
	// a call that finished.
	SpendMeasured SpendBasis = "measured"
	// SpendEstimated means Dashi filled in some usage: the call is still
	// running, was cut short, is uncertain, or its final usage never
	// arrived. A later reconciliation can replace the estimate.
	SpendEstimated SpendBasis = "estimated"
	// SpendNotExecuted means the provider verifiably never ran the request
	// (not dispatched, or rejected before execution), so it costs nothing.
	SpendNotExecuted SpendBasis = "not_executed"
)

// AttemptSpend is what one model attempt counts toward Spend. Spend is the
// sum of a turn's attempts plus local tool charges, so a host that charges
// each attempt (rounding each, or settling a hold by attempt ID) reads these
// rather than re-deriving them from model facts.
type AttemptSpend struct {
	AttemptID string       `json:"attempt_id"`
	Purpose   ModelPurpose `json:"purpose,omitempty"`
	Provider  string       `json:"provider,omitempty"`
	Model     string       `json:"model,omitempty"`
	// Settled means a result or failure is recorded. An unsettled attempt
	// is still running or was lost with its worker; it counts its
	// estimated input until it settles.
	Settled bool       `json:"settled,omitempty"`
	Basis   SpendBasis `json:"basis"`
	// Usage is the usage counted into Spend.Usage, nil when not executed or
	// when it could not be derived.
	Usage *llm.Usage `json:"usage,omitempty"`
	// Charge is the amount counted into Spend.Cost. Nil means zero for a
	// not-executed attempt, and otherwise an unknown charge (an unpriced
	// model, for example), counted in Spend.CostUnknown. A partial charge
	// (Charge.Partial) is a lower bound.
	Charge *llm.Cost `json:"charge,omitempty"`
	// AtRisk is the worst case computed before the call was sent, nil when
	// the call had no cost bound.
	AtRisk *llm.Cost `json:"at_risk,omitempty"`
	// Final means the charge is exact: a measured, complete charge, or zero
	// for an attempt that never ran. A charge that is not final is an
	// estimate, a lower bound, or unknown.
	Final bool `json:"final,omitempty"`
}

// AttemptSpendOf classifies one model attempt from its intent fact and its
// settlement, or nil while the call is pending. It is the same derivation
// TurnView.Attempts uses. A recorder that reserved credits on the
// StepModelIntent step can call it in the commit of the StepModelSettled or
// StepModelReconciled step, with the intent it stored at reservation, to
// settle the hold in the same transaction. A nil intent gives the zero value.
func AttemptSpendOf(intent, settlement *ModelFact) AttemptSpend {
	if intent == nil {
		return AttemptSpend{}
	}
	a, _ := attemptSpendOf(intent, settlement)
	return a
}

// attemptSpendOf is AttemptSpendOf that also reports whether usage
// derivation was incomplete, which sets Spend.UsageIncomplete.
func attemptSpendOf(intent, settlement *ModelFact) (AttemptSpend, bool) {
	a := AttemptSpend{
		AttemptID: intent.AttemptID,
		Purpose:   intent.Purpose,
		Provider:  intent.Provider,
		Model:     intent.Model,
		Settled:   settlement != nil,
		Basis:     SpendMeasured,
	}
	if intent.AtRisk != nil {
		a.AtRisk = new(intent.AtRisk.Clone())
	}
	usage, charge, estimated, counted, incomplete := attemptSpend(intent, settlement)
	if !counted {
		a.Basis = SpendNotExecuted
		a.Final = true
		return a, false
	}
	if estimated {
		a.Basis = SpendEstimated
	}
	a.Usage = usage
	if charge != nil {
		a.Charge = new(charge.Clone())
	}
	a.Final = a.Basis == SpendMeasured && a.Charge != nil && !a.Charge.Partial()
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
