package dashi

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func attemptSpendIntent(priced bool) *ModelFact {
	intent := &ModelFact{
		AttemptID:            "model-1",
		Provider:             "test",
		Model:                "fixture",
		EstimatedInputTokens: 100,
		AtRisk:               &llm.Cost{Total: amount("0.5"), Currency: "USD"},
	}
	if priced {
		intent.ModelInfo = &llm.ModelInfo{Pricing: &newPricedClient().pricing}
	}
	return intent
}

func usageResult(usage *llm.Usage) *llm.Result {
	r := final("answer")
	r.Usage = usage
	return &r
}

func TestAttemptSpendClassifiesEachAttempt(t *testing.T) {
	measured := &llm.Usage{InputTokens: 10, OutputTokens: 5}
	cutStop := usageResult(measured.Copy())
	cutStop.Stop = llm.Stop{}
	tests := []struct {
		name       string
		unpriced   bool
		settlement *ModelFact
		settled    bool
		basis      SpendBasis
		charged    bool
		partial    bool
		final      bool
	}{
		{
			// No recorded Charge: Dashi prices the reported usage.
			name:       "measured",
			settlement: &ModelFact{Result: usageResult(measured.Copy())},
			settled:    true, basis: SpendMeasured, charged: true, final: true,
		},
		{
			name:       "missing usage",
			settlement: &ModelFact{Result: usageResult(nil)},
			settled:    true, basis: SpendEstimated, charged: true,
		},
		{
			name: "interrupted stream",
			settlement: &ModelFact{
				Result:      usageResult(measured.Copy()),
				Interrupted: InterruptRun,
			},
			settled: true, basis: SpendEstimated, charged: true,
		},
		{
			// Usage without a terminal stop may describe an earlier frame.
			name:       "no terminal stop",
			settlement: &ModelFact{Result: cutStop},
			settled:    true, basis: SpendEstimated, charged: true,
		},
		{
			name:       "uncertain",
			settlement: &ModelFact{Error: "timeout", Uncertain: true},
			settled:    true, basis: SpendEstimated, charged: true,
		},
		{
			name: "not dispatched",
			settlement: &ModelFact{
				Error: "offline", Failure: llm.FailureNotDispatched,
				Evidence: "dial failed",
			},
			settled: true, basis: SpendNotExecuted, final: true,
		},
		{
			name: "rejected",
			settlement: &ModelFact{
				Error: "bad request", Failure: llm.FailureRejected,
				Evidence: "400",
			},
			settled: true, basis: SpendNotExecuted, final: true,
		},
		{
			name: "partial hosted cost",
			settlement: &ModelFact{Result: usageResult(&llm.Usage{
				InputTokens: 10, OutputTokens: 5,
				Requests:      map[string]int{llm.MeterCodeExecution: 2},
				RequestsFinal: true,
			})},
			settled: true, basis: SpendMeasured, charged: true, partial: true,
		},
		{
			name:       "unpriced model",
			unpriced:   true,
			settlement: &ModelFact{Result: usageResult(measured.Copy())},
			settled:    true, basis: SpendMeasured,
		},
		{
			name:  "pending",
			basis: SpendEstimated, charged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			intent := attemptSpendIntent(!tc.unpriced)
			if tc.settlement != nil {
				tc.settlement.AttemptID = intent.AttemptID
			}
			got, _ := attemptSpendOf(intent, tc.settlement)
			if got.AttemptID != "model-1" || got.Provider != "test" ||
				got.Model != "fixture" {
				t.Fatalf("identity=%+v", got)
			}
			if got.Settled != tc.settled || got.Basis != tc.basis ||
				(got.Charge != nil) != tc.charged || got.Final != tc.final {
				t.Fatalf("got %+v", got)
			}
			if tc.charged && got.Charge.Partial() != tc.partial {
				t.Fatalf("partial=%t", got.Charge.Partial())
			}
			if got.AtRisk == nil || got.AtRisk.Total != amount("0.5") {
				t.Fatalf("at risk=%+v", got.AtRisk)
			}
			if tc.basis == SpendNotExecuted && got.Usage != nil {
				t.Fatalf("not-executed usage=%+v", got.Usage)
			}
		})
	}
}

func TestAttemptsSumToSpend(t *testing.T) {
	first := callResult("lookup-1", "lookup")
	first.Usage = &llm.Usage{InputTokens: 10, OutputTokens: 10}
	client := newPricedClient(first, final("done"))
	out := runBudgetTurn(
		t, client, nil, llm.Settings{MaxTokens: 100},
		tool.List{budgetLookup(nil)}, Hooks{},
	)
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 2 {
		t.Fatalf("attempts=%+v", view.Attempts)
	}
	var ids []string
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent {
			ids = append(ids, step.Model.AttemptID)
		}
	}
	for i, a := range view.Attempts {
		if a.AttemptID != ids[i] || !a.Settled {
			t.Fatalf("attempt %d=%+v, want %s", i, a, ids[i])
		}
	}
	if !view.Attempts[0].Final || view.Attempts[1].Basis != SpendEstimated ||
		view.Attempts[1].Final {
		t.Fatalf("attempts=%+v", view.Attempts)
	}
	sum := map[string]money.Amount{}
	var usage llm.Usage
	for _, a := range view.Attempts {
		sum[a.Charge.Currency] += a.Charge.Total
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
	if !reflect.DeepEqual(decoded.Attempts, view.Attempts) {
		t.Fatalf("attempts changed through JSON:\n%+v\n%+v",
			decoded.Attempts, view.Attempts)
	}
}

func TestAttemptsIncludePendingAttempt(t *testing.T) {
	view, err := Replay(interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 1 {
		t.Fatalf("attempts=%+v", view.Attempts)
	}
	a := view.Attempts[0]
	if a.AttemptID != "model-1" || a.Settled || a.Final ||
		a.Basis != SpendEstimated {
		t.Fatalf("pending attempt=%+v", a)
	}
	if view.Spend.Estimated != 1 || view.Spend.CostUnknown != 1 {
		t.Fatalf("spend=%+v", view.Spend)
	}
}
