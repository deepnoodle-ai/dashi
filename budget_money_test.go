package kernel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
)

// TestBudgetCostCapBoundaryIsExact keeps the existing rule, a call whose
// worst case reaches the cap exactly is admitted, and checks that one nano
// more is refused.
func TestBudgetCostCapBoundaryIsExact(t *testing.T) {
	client := newPricedClient()
	request, err := client.Prepare(
		context.Background(),
		llm.Input{Model: "fixture", Settings: llm.Settings{MaxTokens: 100}},
	)
	if err != nil {
		t.Fatal(err)
	}
	risk, ok := llm.MaxCostAtRisk(request, client.pricing)
	if !ok {
		t.Fatal("priced request had no cost bound")
	}
	used := amount("0.1")
	check := func(cap money.Amount) *Stop {
		state := &replayState{TurnView: TurnView{
			Budget: &Budget{MaxCost: &Money{Amount: cap, Currency: "USD"}},
			Spend: Spend{
				Cost: map[string]money.Amount{"USD": used},
			},
		}}
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
	if stop.Limit.CostAtRisk != risk.Total || stop.Limit.CostUsed != used {
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
		state := &replayState{TurnView: TurnView{
			Budget: &Budget{MaxCost: &Money{Amount: cap, Currency: "USD"}},
			Spend: Spend{
				Cost: map[string]money.Amount{"USD": tc.used},
			},
		}}
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
	var legacy Budget
	if err := json.Unmarshal(
		[]byte(`{"max_cost":{"amount":0.5,"currency":"USD"}}`),
		&legacy,
	); err != nil || legacy.MaxCost.Amount != amount("0.5") {
		t.Fatalf("legacy budget = %+v, %v", legacy, err)
	}
}

func TestLimitDetailJSON(t *testing.T) {
	detail := LimitDetail{
		Kind:       LimitCost,
		CostLimit:  amount("0.01"),
		CostUsed:   amount("0.0042"),
		CostAtRisk: amount("0.006"),
		FitTokens:  12,
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var restored LimitDetail
	if err := json.Unmarshal(raw, &restored); err != nil || restored != detail {
		t.Fatalf("round trip %s = %+v, %v", raw, restored, err)
	}

	var legacy LimitDetail
	err = json.Unmarshal(
		[]byte(`{"kind":"cost","limit":0.01,"used":0.0042,"at_risk":0.006,`+
			`"fit_tokens":12}`),
		&legacy,
	)
	if err != nil || legacy != detail {
		t.Fatalf("legacy cost detail = %+v, %v", legacy, err)
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
