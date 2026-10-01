package dashi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestSpendString(t *testing.T) {
	tests := []struct {
		name  string
		spend Spend
		want  string
	}{
		{
			name: "zero value",
			want: "no recorded cost",
		},
		{
			name: "empty maps",
			spend: Spend{
				Cost:     map[string]money.Amount{},
				ToolCost: map[string]money.Amount{},
			},
			want: "no recorded cost",
		},
		{
			name:  "tokens without cost",
			spend: Spend{Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}, Estimated: 1},
			want:  "no recorded cost",
		},
		{
			name:  "explicit zero",
			spend: Spend{Cost: map[string]money.Amount{"USD": 0}},
			want:  "model: 0 USD",
		},
		{
			name:  "nano precision",
			spend: Spend{Cost: map[string]money.Amount{"USD": 1}},
			want:  "model: 0.000000001 USD",
		},
		{
			name:  "tool only",
			spend: Spend{ToolCost: map[string]money.Amount{"EUR": amount("2.5")}},
			want:  "tools: 2.5 EUR",
		},
		{
			name: "sorted separate currencies",
			spend: Spend{
				Cost: map[string]money.Amount{
					"USD": amount("1"), "EUR": amount("0.5"),
				},
				ToolCost: map[string]money.Amount{
					"USD": amount("2"), "CREDITS": amount("3"),
				},
			},
			want: "model: 0.5 EUR, 1 USD; tools: 3 CREDITS, 2 USD",
		},
		{
			name: "signed bounds",
			spend: Spend{Cost: map[string]money.Amount{
				"MAX": money.Amount(math.MaxInt64), "MIN": money.Amount(math.MinInt64),
			}},
			want: "model: 9223372036.854775807 MAX, -9223372036.854775808 MIN",
		},
		{
			name:  "unknown only",
			spend: Spend{CostUnknown: 1},
			want:  "no recorded cost; unknown: 1",
		},
		{
			name:  "partial only",
			spend: Spend{CostPartial: 1},
			want:  "no recorded cost; partial: 1",
		},
		{
			name:  "overlapping counters without amounts",
			spend: Spend{CostPartial: 2, CostUnknown: 3},
			want:  "no recorded cost; partial: 2; unknown: 3",
		},
		{
			name: "partial model with complete tools",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostPartial: 1,
			},
			want: "model: at least 0.5 USD; tools: 2 USD; partial: 1",
		},
		{
			name: "unknown qualifies both categories",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostUnknown: 1,
			},
			want: "model: at least 0.5 USD; tools: at least 2 USD; unknown: 1",
		},
		{
			name: "overlapping counters keep separate labels",
			spend: Spend{
				Cost:        map[string]money.Amount{"USD": amount("0.5")},
				CostPartial: 1,
				CostUnknown: 1,
			},
			want: "model: at least 0.5 USD; partial: 1; unknown: 1",
		},
		{
			name: "unknown tool zero remains explicit",
			spend: Spend{
				ToolCost:    map[string]money.Amount{"USD": 0},
				CostUnknown: 1,
			},
			want: "tools: at least 0 USD; unknown: 1",
		},
		{
			name: "partial without model amounts does not qualify tools",
			spend: Spend{
				ToolCost:    map[string]money.Amount{"USD": amount("2")},
				CostPartial: 1,
			},
			want: "tools: 2 USD; partial: 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spend.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpendStringToolOverflow(t *testing.T) {
	most := money.Amount(math.MaxInt64)
	spend := Spend{
		Cost:     map[string]money.Amount{"USD": amount("0.5")},
		ToolCost: map[string]money.Amount{"USD": most},
	}.Add(Spend{ToolCost: map[string]money.Amount{"USD": 1}})
	if spend.CostUnknown != 1 || spend.ToolCost["USD"] != most {
		t.Fatal("overflow must retain the known tool amount and mark cost unknown")
	}
	want := "model: at least 0.5 USD; tools: at least 9223372036.854775807 USD; unknown: 1"
	if got := spend.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSpendStringUsageOverflow(t *testing.T) {
	initial := Spend{
		Usage: llm.Usage{
			InputTokens:      math.MaxInt,
			Requests:         map[string]int{llm.MeterWebSearch: 1},
			RequestsComplete: true,
		},
		Cost:     map[string]money.Amount{"USD": amount("0.5")},
		ToolCost: map[string]money.Amount{"USD": amount("2")},
	}
	addition := Spend{
		Usage:    llm.Usage{InputTokens: 1},
		Cost:     map[string]money.Amount{"USD": amount("0.25")},
		ToolCost: map[string]money.Amount{"USD": amount("3")},
	}
	// Both inputs are valid. Only their usage sum exceeds the counter limit.
	for _, usage := range []llm.Usage{initial.Usage, addition.Usage} {
		if err := usage.Validate(); err != nil {
			t.Fatalf("invalid test usage: %v", err)
		}
	}
	spend := initial.Add(addition)
	if spend.Cost["USD"] != amount("0.75") || spend.ToolCost["USD"] != amount("5") {
		t.Fatal("usage overflow must retain both exact monetary sums")
	}
	if !reflect.DeepEqual(spend.Usage, initial.Usage) {
		t.Fatal("usage overflow must retain the original usage")
	}
	if !spend.UsageIncomplete || spend.CostUnknown != 0 ||
		spend.CostPartial != 0 {
		t.Fatal("usage overflow must be distinct from monetary uncertainty")
	}
	want := "model: 0.75 USD; tools: 5 USD; usage incomplete"
	if got := spend.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSpendStringStableAndPreservesFields(t *testing.T) {
	spend := Spend{
		Usage: llm.Usage{
			InputTokens: 10, OutputTokens: 2,
			Requests: map[string]int{llm.MeterCodeExecution: 1},
		},
		Cost:             map[string]money.Amount{"USD": amount("1"), "EUR": amount("0.5")},
		ToolCost:         map[string]money.Amount{"USD": amount("2"), "EUR": amount("3")},
		CostPartial:      1,
		CostUnknown:      2,
		Estimated:        3,
		UnpricedRequests: map[string]int{llm.MeterCodeExecution: 1},
	}
	before := spend.Clone()
	want := "model: at least 0.5 EUR, 1 USD; tools: at least 3 EUR, 2 USD; partial: 1; unknown: 2"
	for range 100 {
		if got := spend.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
	if !reflect.DeepEqual(spend, before) {
		t.Fatal("formatting modified Spend fields")
	}
	// Both values and pointers support the standard fmt display verbs.
	for _, value := range []fmt.Stringer{spend, &spend} {
		for _, verb := range []string{"%s", "%v"} {
			if got := fmt.Sprintf(verb, value); got != want {
				t.Fatalf("fmt.Sprintf(%q) = %q, want %q", verb, got, want)
			}
		}
	}
}

// TestBudgetCostCapBoundaryIsExact keeps the existing rule, a call whose
// worst case reaches the cap exactly is allowed, and checks that one nano
// more is refused.
func TestBudgetCostCapBoundaryIsExact(t *testing.T) {
	client := newPricedClient()
	request, err := client.Prepare(
		t.Context(),
		llm.Input{Model: "fixture", Settings: llm.Settings{MaxTokens: 100}},
	)
	if err != nil {
		t.Fatal(err)
	}
	risk, ok := llm.WorstCaseCost(request, client.pricing)
	if !ok {
		t.Fatal("priced request had no cost bound")
	}
	used := amount("0.1")
	check := func(cap money.Amount) *Stop {
		state := &replayState{
			TurnView: TurnView{
				Budget: &Budget{MaxCost: &Money{Amount: cap, Currency: "USD"}},
				Spend:  Spend{Cost: map[string]money.Amount{"USD": used}},
			},
		}
		_, remaining, stop := checkSendBudget(state, request, "fixture")
		if stop == nil && (remaining == nil || remaining.Amount != cap-used) {
			t.Fatalf("remaining=%+v, want %s", remaining, cap-used)
		}
		return stop
	}
	if stop := check(used + risk.Total); stop != nil {
		t.Fatalf("a call reaching the cap exactly was refused: %+v", stop)
	}
	stop := check(used + risk.Total - 1)
	if stop == nil || stop.Limit == nil || stop.Limit.Kind != LimitCost {
		t.Fatalf("a call one nano over the cap was admitted: %+v", stop)
	}
	if stop.Limit.WorstCaseCost != risk.Total || stop.Limit.CostUsed != used ||
		stop.Limit.Currency != "USD" {
		t.Fatalf("limit detail=%+v", stop.Limit)
	}
}

// TestBudgetSpentAtExactCapStops keeps the existing rule that spend equal to
// the cap ends the turn before another call, and spend below it does not.
func TestBudgetSpentAtExactCapStops(t *testing.T) {
	cap := amount("0.5")
	for _, tc := range []struct {
		used money.Amount
		stop bool
	}{
		{cap - 1, false},
		{cap, true},
		{cap + 1, true},
	} {
		state := &replayState{
			TurnView: TurnView{
				Budget: &Budget{MaxCost: &Money{Amount: cap, Currency: "USD"}},
				Spend:  Spend{Cost: map[string]money.Amount{"USD": tc.used}},
			},
		}
		stop := budgetSpentStop(state)
		if (stop != nil) != tc.stop {
			t.Fatalf("used %s of %s: stop=%+v", tc.used, cap, stop)
		}
	}
}

func TestSpendSumsChargesExactly(t *testing.T) {
	totals := map[string]money.Amount{}
	for range 10 {
		if err := addCurrencyCost(totals, "USD", amount("0.1")); err != nil {
			t.Fatal(err)
		}
	}
	if totals["USD"] != amount("1") {
		t.Fatalf("ten dimes = %s", totals["USD"])
	}
}

func TestBudgetJSONUsesDecimalStrings(t *testing.T) {
	budget := Budget{MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"}}
	raw, err := json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"max_cost":{"amount":"0.5","currency":"USD"}}` {
		t.Fatalf("encoded %s", raw)
	}
	var numeric Budget
	if err := json.Unmarshal(
		[]byte(`{"max_cost":{"amount":0.5,"currency":"USD"}}`),
		&numeric,
	); err != nil || numeric.MaxCost.Amount != amount("0.5") {
		t.Fatalf("numeric budget = %+v, %v", numeric, err)
	}
}

func TestLimitDetailJSON(t *testing.T) {
	detail := LimitDetail{
		Kind:                     LimitCost,
		CostLimit:                amount("0.01"),
		CostUsed:                 amount("0.0042"),
		WorstCaseCost:            amount("0.006"),
		Currency:                 "USD",
		SuggestedMaxOutputTokens: 12,
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var restored LimitDetail
	if err := json.Unmarshal(raw, &restored); err != nil || restored != detail {
		t.Fatalf("round trip %s = %+v, %v", raw, restored, err)
	}

	var tokens LimitDetail
	err = json.Unmarshal(
		[]byte(`{"kind":"output_tokens","limit":100,"used":120}`),
		&tokens,
	)
	if err != nil || tokens.Limit != 100 || tokens.Used != 120 {
		t.Fatalf("count detail = %+v, %v", tokens, err)
	}
	err = json.Unmarshal(
		[]byte(`{"kind":"output_tokens","limit":100.5}`),
		&tokens,
	)
	if err == nil {
		t.Fatal("a fractional count was accepted")
	}
}

// creditDenialHooks deny a call with a host limit that reports money.
func creditDenialHooks(currency string) Hooks {
	return SendHook{
		Name: "credits",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			return SendDecision{
				Reason: "insufficient credits",
				Limit: &LimitDetail{
					Kind:          LimitHost,
					Code:          "insufficient_credits",
					CostLimit:     amount("0.002"),
					WorstCaseCost: plan.WorstCaseCost.Total,
					Currency:      currency,
				},
			}, nil
		},
	}.Hooks()
}

func TestHostDenialCarriesMoneyWithCurrency(t *testing.T) {
	out := runBudgetTurn(
		t,
		newPricedClient(final("unused")),
		nil,
		llm.Settings{MaxTokens: 100},
		nil,
		creditDenialHooks("CREDIT"),
	)
	limit := requireBudgetLimit(t, out, LimitHost)
	if limit.Currency != "CREDIT" || limit.CostLimit != amount("0.002") ||
		limit.WorstCaseCost <= 0 || limit.Limit != 0 || limit.Used != 0 {
		t.Fatalf("host limit=%+v", limit)
	}

	out = runBudgetTurn(
		t,
		newPricedClient(final("unused")),
		nil,
		llm.Settings{MaxTokens: 100},
		nil,
		creditDenialHooks(""),
	)
	if out.Stop.Kind != StopFailed || out.Stop.Reason != StopReasonHookFailed {
		t.Fatalf("host money without a currency was accepted: %+v", out.Stop)
	}
}

func modelCallCostStart(priced bool) *ModelFact {
	callStart := &ModelFact{
		ModelCallID:          "model-1",
		Provider:             "test",
		Model:                "fixture",
		EstimatedInputTokens: 100,
		WorstCaseCost:        &llm.Cost{Total: amount("0.5"), Currency: "USD"},
	}
	if priced {
		callStart.ModelInfo = &llm.ModelInfo{Pricing: &newPricedClient().pricing}
	}
	return callStart
}

func usageResult(usage *llm.Usage) *llm.Result {
	r := final("answer")
	r.Usage = usage
	return &r
}

func TestModelCallCostClassifiesEachModelCall(t *testing.T) {
	measured := &llm.Usage{InputTokens: 10, OutputTokens: 5}
	cutStop := usageResult(measured.Copy())
	cutStop.Stop = llm.Stop{}
	tests := []struct {
		name     string
		unpriced bool
		finish   *ModelFact
		finished bool
		basis    UsageSource
		charged  bool
		partial  bool
		final    bool
	}{
		{
			// No recorded Cost: Dashi prices the reported usage.
			name:     "measured",
			finish:   &ModelFact{Result: usageResult(measured.Copy())},
			finished: true, basis: UsageSourceReported, charged: true, final: true,
		},
		{
			name:     "missing usage",
			finish:   &ModelFact{Result: usageResult(nil)},
			finished: true, basis: UsageSourceEstimated, charged: true,
		},
		{
			name: "interrupted stream",
			finish: &ModelFact{
				Result:      usageResult(measured.Copy()),
				Interrupted: InterruptRun,
			},
			finished: true, basis: UsageSourceEstimated, charged: true,
		},
		{
			// Usage without a final stop reason may describe an earlier frame.
			name:     "no terminal stop",
			finish:   &ModelFact{Result: cutStop},
			finished: true, basis: UsageSourceEstimated, charged: true,
		},
		{
			name:     "uncertain",
			finish:   &ModelFact{Error: "timeout", ResultUnknown: true},
			finished: true, basis: UsageSourceEstimated, charged: true,
		},
		{
			name: "not dispatched",
			finish: &ModelFact{
				Error: "offline", SendStatus: llm.SendStatusNotSent,
				Evidence: "dial failed",
			},
			finished: true, basis: UsageSourceNotSent, final: true,
		},
		{
			name: "rejected",
			finish: &ModelFact{
				Error: "bad request", SendStatus: llm.SendStatusProviderRejected,
				Evidence: "400",
			},
			finished: true, basis: UsageSourceNotSent, final: true,
		},
		{
			name: "partial provider tool cost",
			finish: &ModelFact{Result: usageResult(&llm.Usage{
				InputTokens: 10, OutputTokens: 5,
				Requests:         map[string]int{llm.MeterCodeExecution: 2},
				RequestsComplete: true,
			})},
			finished: true, basis: UsageSourceReported, charged: true, partial: true,
		},
		{
			name:     "unpriced model",
			unpriced: true,
			finish:   &ModelFact{Result: usageResult(measured.Copy())},
			finished: true, basis: UsageSourceReported,
		},
		{
			name:  "pending",
			basis: UsageSourceEstimated, charged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callStart := modelCallCostStart(!tc.unpriced)
			if tc.finish != nil {
				tc.finish.ModelCallID = callStart.ModelCallID
			}
			got := ModelCallCostOf(callStart, tc.finish)
			if got.ModelCallID != "model-1" || got.Provider != "test" ||
				got.Model != "fixture" {
				t.Fatalf("identity=%+v", got)
			}
			if got.Finished != tc.finished || got.UsageSource != tc.basis ||
				(got.Cost != nil) != tc.charged || got.Exact != tc.final {
				t.Fatalf("got %+v", got)
			}
			if tc.charged && got.Cost.Partial() != tc.partial {
				t.Fatalf("partial=%t", got.Cost.Partial())
			}
			if got.WorstCaseCost == nil || got.WorstCaseCost.Total != amount("0.5") {
				t.Fatalf("at risk=%+v", got.WorstCaseCost)
			}
			if tc.basis == UsageSourceNotSent && got.Usage != nil {
				t.Fatalf("not-executed usage=%+v", got.Usage)
			}
		})
	}
}

func TestModelCallCostsSumToSpend(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	client := newPricedClient(first, final("done"))
	out := runBudgetTurn(
		t, client, nil, llm.Settings{MaxTokens: 100},
		tool.List{budgetLookup(nil)}, Hooks{},
	)
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ModelCallCosts) != 2 {
		t.Fatalf("attempts=%+v", view.ModelCallCosts)
	}
	var ids []string
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallStarted {
			ids = append(ids, step.Model.ModelCallID)
		}
	}
	for i, a := range view.ModelCallCosts {
		if a.ModelCallID != ids[i] || !a.Finished {
			t.Fatalf("attempt %d=%+v, want %s", i, a, ids[i])
		}
	}
	if !view.ModelCallCosts[0].Exact || view.ModelCallCosts[1].UsageSource != UsageSourceEstimated ||
		view.ModelCallCosts[1].Exact {
		t.Fatalf("attempts=%+v", view.ModelCallCosts)
	}
	callStarts := map[string]*ModelFact{}
	for _, step := range out.Turn.Steps {
		switch step.Kind {
		case StepModelCallStarted:
			callStarts[step.Model.ModelCallID] = step.Model
		case StepModelCallFinished:
			got := ModelCallCostOf(callStarts[step.Model.ModelCallID], step.Model)
			var want ModelCallCost
			for _, a := range view.ModelCallCosts {
				if a.ModelCallID == step.Model.ModelCallID {
					want = a
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ModelCallCostOf=%+v, view=%+v", got, want)
			}
		}
	}
	if (ModelCallCostOf(nil, nil) != ModelCallCost{}) {
		t.Fatal("nil intent is not the zero value")
	}
	sum := map[string]money.Amount{}
	var usage llm.Usage
	for _, a := range view.ModelCallCosts {
		sum[a.Cost.Currency] += a.Cost.Total
		if err := usage.Add(a.Usage); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(sum, view.Spend.Cost) ||
		!reflect.DeepEqual(usage, view.Spend.Usage) ||
		view.Spend.Estimated != 1 {
		t.Fatalf("attempts sum %v %+v, spend %+v", sum, usage, view.Spend)
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TurnView
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.ModelCallCosts, view.ModelCallCosts) {
		t.Fatalf("attempts changed through JSON:\n%+v\n%+v",
			decoded.ModelCallCosts, view.ModelCallCosts)
	}
}

func TestModelCallCostsIncludePendingModelCall(t *testing.T) {
	view, err := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
	).View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ModelCallCosts) != 1 {
		t.Fatalf("attempts=%+v", view.ModelCallCosts)
	}
	a := view.ModelCallCosts[0]
	if a.ModelCallID != "model-1" || a.Finished || a.Exact ||
		a.UsageSource != UsageSourceEstimated {
		t.Fatalf("pending attempt=%+v", a)
	}
	if view.Spend.Estimated != 1 || view.Spend.CostUnknown != 1 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}

// Canonical JSON compares strings by content, so "0.50" and "0.5" differ.
// Dashi hashes Go values, and money.Amount always marshals one trimmed
// spelling, so every spelling a store or a caller may supply decodes to a
// value with the same hash.
func TestMoneyHashIgnoresStoredSpelling(t *testing.T) {
	if canonjson.Equal(json.RawMessage(`"0.50"`), json.RawMessage(`"0.5"`)) {
		t.Fatal("canonical JSON unexpectedly equates decimal strings")
	}
	live := Budget{MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"}}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"max_cost":{"amount":"0.5","currency":"USD"}}` {
		t.Fatalf("live budget encoded %s", raw)
	}
	want := canonjson.Hash(live)
	if got := canonjson.Hash(jsonbRoundTrip(live)); got != want {
		t.Fatal("a jsonb round trip changed the budget digest")
	}
	for _, spelling := range []string{
		`"0.5"`,
		`"0.50"`,
		`"0.500000000"`,
		`"00.5"`,
		`0.5`,
		`0.50`,
		`5e-1`,
		`0.5000000000001`,
	} {
		var stored Budget
		text := `{"max_cost":{"amount":` + spelling + `,"currency":"USD"}}`
		if err := json.Unmarshal([]byte(text), &stored); err != nil {
			t.Fatalf("decode %s: %v", spelling, err)
		}
		if canonjson.Hash(stored) != want {
			t.Fatalf("stored amount %s changed the budget digest", spelling)
		}
	}
}

// TestJSONBRoundTripBudgetTurn runs a capped turn through a jsonb-style
// recorder, then retries its Start from the reloaded turn, once with the live
// budget and once with the budget spelled as bare JSON numbers.
func TestJSONBRoundTripBudgetTurn(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 40}
	rec := jsonbRecorder{NewMemoryRecorder()}
	def := testAgent()
	def.Settings.MaxTokens = 100
	runner := Runner{
		Client:   newPricedClient(answer),
		Recorder: rec,
	}
	start := Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		Budget:    &Budget{MaxCost: &Money{Amount: amount("0.50"), Currency: "USD"}},
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		start,
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	// Typed money fields decode to the same values, so the reloaded steps
	// re-encode identically; reload's check for changed bytes does not apply.
	turn, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Cost["USD"] != amount("0.00014") ||
		view.Budget.MaxCost.Amount != amount("0.5") {
		t.Fatalf("reloaded spend=%+v budget=%+v", view.Spend, view.Budget)
	}

	var numeric Budget
	if err := json.Unmarshal(
		[]byte(`{"max_cost":{"amount":0.50,"currency":"USD"}}`),
		&numeric,
	); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []*Budget{start.Budget, &numeric} {
		retry := start
		retry.Budget = budget
		again, err := runner.Run(t.Context(), turn, retry)
		if err != nil || again.Stop.Kind != StopCompleted {
			t.Fatalf("retried start=%+v err=%v", again.Stop, err)
		}
	}
}
