package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestSpendAddUsageFailuresPreserveIndependentMoney(t *testing.T) {
	tests := []struct {
		name          string
		first, second llm.Usage
	}{
		{
			name:   "disjoint total",
			first:  llm.Usage{InputTokens: math.MaxInt - 1},
			second: llm.Usage{OutputTokens: 2},
		},
		{
			name:   "requests",
			first:  llm.Usage{Requests: map[string]int{"search": math.MaxInt}},
			second: llm.Usage{Requests: map[string]int{"search": 1}},
		},
		{
			name: "modality",
			first: llm.Usage{ModalityTokens: map[string]llm.ModalityTokenUsage{
				"audio": {InputTokens: math.MaxInt},
			}},
			second: llm.Usage{ModalityTokens: map[string]llm.ModalityTokenUsage{
				"audio": {OutputTokens: 1},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, u := range []llm.Usage{tc.first, tc.second} {
				if err := u.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			first := Spend{
				Usage:    tc.first,
				Cost:     map[string]money.Amount{"USD": amount("0.5")},
				ToolCost: map[string]money.Amount{"USD": amount("2")},
			}
			second := Spend{
				Usage:    tc.second,
				Cost:     map[string]money.Amount{"USD": amount("0.25")},
				ToolCost: map[string]money.Amount{"USD": amount("3")},
			}
			beforeFirst, beforeSecond := first.Clone(), second.Clone()
			sum := first.Add(second)
			if !sum.UsageIncomplete {
				t.Fatal("rejected usage contribution is not observable")
			}
			if !reflect.DeepEqual(sum.Usage, first.Usage) {
				t.Fatal("failed usage addition changed the prior usage")
			}
			if sum.Cost["USD"] != amount("0.75") ||
				sum.ToolCost["USD"] != amount("5") {
				t.Fatal("usage failure lost independent monetary sums")
			}
			if sum.CostUnknown != 0 || sum.CostPartial != 0 {
				t.Fatal("usage failure incorrectly marked money uncertain")
			}
			if !reflect.DeepEqual(first, beforeFirst) ||
				!reflect.DeepEqual(second, beforeSecond) {
				t.Fatal("Add changed an operand")
			}
			if !sum.Clone().Add(Spend{}).UsageIncomplete ||
				!(Spend{}).Add(sum).UsageIncomplete {
				t.Fatal("copy or zero addition cleared incomplete usage")
			}
		})
	}
}

func TestSpendAddSaturatesDiagnosticCounters(t *testing.T) {
	first := Spend{
		Estimated:        math.MaxInt,
		CostPartial:      math.MaxInt,
		CostUnknown:      math.MaxInt,
		UnpricedRequests: map[string]int{"search": math.MaxInt},
		Cost:             map[string]money.Amount{"USD": money.Amount(math.MaxInt64)},
		ToolCost:         map[string]money.Amount{"USD": money.Amount(math.MaxInt64)},
	}
	sum := first.Add(Spend{
		Estimated:        1,
		CostPartial:      1,
		CostUnknown:      1,
		UnpricedRequests: map[string]int{"search": 1},
		Cost:             map[string]money.Amount{"USD": 1, "EUR": 2},
		ToolCost:         map[string]money.Amount{"USD": 1, "EUR": 3},
	})
	for _, count := range []int{
		sum.Estimated, sum.CostPartial, sum.CostUnknown,
		sum.UnpricedRequests["search"],
	} {
		if count != math.MaxInt {
			t.Fatalf("diagnostic counter wrapped: %d", count)
		}
	}
	if sum.Cost["USD"] != first.Cost["USD"] ||
		sum.ToolCost["USD"] != first.ToolCost["USD"] {
		t.Fatal("monetary overflow changed the retained subtotal")
	}
	if sum.Cost["EUR"] != 2 || sum.ToolCost["EUR"] != 3 {
		t.Fatal("monetary overflow lost independent currency additions")
	}
	if _, known := sum.costIn("USD"); known {
		t.Fatal("counter overflow made money suitable for admission")
	}
	if sum.UsageIncomplete {
		t.Fatal("monetary overflow marked usage incomplete")
	}
}

func TestSpendJSONPreservesNewAndLegacyMeanings(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{},"cost":{"USD":"0.5"},"cost_unknown":1}`,
		`{"usage":{},"cost":{"USD":"0.5"},"usage_incomplete":true}`,
		`{"usage":{},"cost_unknown":1,"usage_incomplete":true}`,
		`{"usage":{}}`,
	} {
		var first, second Spend
		if err := json.Unmarshal([]byte(raw), &first); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &second); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatal("Spend round trip changed accounting flags")
		}
		if strings.Contains(raw, "cost_unknown") && second.CostUnknown != 1 {
			t.Fatal("legacy ambiguity was reclassified")
		}
		if !first.UsageIncomplete && strings.Contains(string(encoded),
			"usage_incomplete") {
			t.Fatal("zero value added a new storage flag")
		}
	}
}

func accountingRecord(usages ...llm.Usage) Turn {
	steps := []Step{interruptionStart(testAgent())}
	for i, usage := range usages {
		id := fmt.Sprintf("model-%d", i+1)
		intent := interruptionModelIntent(id)
		intent.Model.MessageID = id + ":message"
		result := final("answer")
		result.Usage = usage.Copy()
		steps = append(steps, intent, Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: id,
				MessageID:   id + ":message",
				Result:      &result,
				Cost: &llm.Cost{
					Total:    amount("0.25"),
					Currency: "USD",
					Source:   llm.CostSourceProviderReported,
				},
			},
		})
	}
	return interruptionRecord(steps...)
}

func TestReplayKeepsValidOverflowingSettlements(t *testing.T) {
	turn := accountingRecord(
		llm.Usage{InputTokens: math.MaxInt - 1},
		llm.Usage{OutputTokens: 2},
		llm.Usage{OutputTokens: 1},
	)
	recorder := NewMemoryRecorder()
	for _, step := range turn.Steps {
		if err := recorder.Commit(t.Context(), CommitRequest{
			Step:        step,
			ExpectedSeq: step.Seq - 1,
		}); err != nil {
			t.Fatalf("valid settlement rejected: %v", err)
		}
	}
	saved, err := recorder.Load(t.Context(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Spend.UsageIncomplete || view.Spend.CostUnknown != 0 {
		t.Fatal("replay conflated usage overflow and monetary uncertainty")
	}
	if view.Spend.Cost["USD"] != amount("0.75") {
		t.Fatal("replay lost independent charges")
	}
	if view.Spend.Usage.InputTokens != math.MaxInt-1 ||
		view.Spend.Usage.OutputTokens != 1 {
		t.Fatal("replay lost lexical retained subtotals or later fitting usage")
	}
}

func TestReplayMonetaryOverflowRemainsUncertain(t *testing.T) {
	turn := accountingRecord(llm.Usage{}, llm.Usage{})
	turn.Steps[2].Model.Cost.Total = money.Amount(math.MaxInt64)
	turn.Steps[4].Model.Cost.Total = 1
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Cost["USD"] != money.Amount(math.MaxInt64) ||
		view.Spend.CostUnknown != 1 {
		t.Fatal("replay lost monetary overflow uncertainty")
	}
	if view.Spend.UsageIncomplete {
		t.Fatal("monetary overflow incorrectly marked usage incomplete")
	}
}

func TestReplayUnpricedRequestsSaturateWithoutDroppingEvidence(t *testing.T) {
	turn := accountingRecord(llm.Usage{}, llm.Usage{})
	turn.Steps[2].Model.Cost.UnpricedRequests = map[string]int{
		llm.MeterUnreported: math.MaxInt,
	}
	turn.Steps[4].Model.Cost.UnpricedRequests = map[string]int{
		llm.MeterUnreported: 1,
	}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.UnpricedRequests[llm.MeterUnreported] != math.MaxInt ||
		view.Spend.CostPartial != 2 || view.Spend.CostUnknown != 0 ||
		view.Spend.Cost["USD"] != amount("0.5") {
		t.Fatal("unpriced request overflow lost monetary evidence")
	}
	if view.Spend.UsageIncomplete {
		t.Fatal("diagnostic saturation changed valid usage completeness")
	}
}

func TestReplayRejectsMalformedIndividualAccounting(t *testing.T) {
	for _, corrupt := range []func(*ModelFact){
		func(f *ModelFact) { f.Result.Usage.InputTokens = -1 },
		func(f *ModelFact) { f.Cost.Total = -1 },
		func(f *ModelFact) { f.Cost.Currency = "" },
	} {
		turn := accountingRecord(llm.Usage{})
		corrupt(turn.Steps[2].Model)
		if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
			t.Fatalf("invalid individual accounting accepted: %v", err)
		}
	}
}

func TestReplayDerivedUsageOverflowPreservesRecordedCharge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		usage        llm.Usage
		fallback     int
		providerTool bool
	}{
		{
			name: "unreported requests",
			usage: llm.Usage{
				Requests: map[string]int{llm.MeterUnreported: math.MaxInt},
			},
			providerTool: true,
		},
		{
			name:     "fallback tokens",
			usage:    llm.Usage{OutputTokens: 2},
			fallback: math.MaxInt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := accountingRecord(tc.usage)
			intent, settled := turn.Steps[1].Model, turn.Steps[2].Model
			intent.EstimatedInputTokens = tc.fallback
			pricing := newPricedClient().pricing
			intent.ModelInfo = &llm.ModelInfo{Pricing: &pricing}
			if tc.providerTool {
				intent.Tools = []tool.Definition{{
					Name: "search", Kind: tool.KindProviderTool,
					ProviderTool: &tool.ProviderToolSpec{
						Provider: "test", API: "test.chat", Type: "web_search",
					},
				}}
			} else {
				settled.ResultUnknown = true
				settled.Error = "connection lost"
			}
			view, err := Replay(turn)
			if err != nil {
				t.Fatal(err)
			}
			if !view.Spend.UsageIncomplete || view.Spend.CostUnknown != 0 {
				t.Fatal("valid-fact enrichment failure lost its separate signal")
			}
			want := tc.usage.Copy()
			want.RequestsComplete = false
			if !reflect.DeepEqual(view.Spend.Usage, *want) {
				t.Fatal("failed enrichment changed the valid recorded usage")
			}
			if view.Spend.Cost["USD"] != amount("0.25") {
				t.Fatal("failed enrichment lost the independent charge")
			}
			settled.Cost = nil
			view, err = Replay(turn)
			if err != nil || view.Spend.CostUnknown != 1 {
				t.Fatalf("missing independent charge did not remain unknown: %v", err)
			}
		})
	}
}

func TestReplayDerivedOverflowWithoutReportedUsageOmitsFallback(t *testing.T) {
	turn := accountingRecord(llm.Usage{})
	turn.Steps[1].Model.EstimatedInputTokens = math.MaxInt
	turn.Steps[2].Model.Result.Usage = nil
	pricing := newPricedClient().pricing
	turn.Steps[1].Model.ModelInfo = &llm.ModelInfo{Pricing: &pricing}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Spend.UsageIncomplete || !reflect.DeepEqual(view.Spend.Usage,
		llm.Usage{}) || view.Spend.Cost["USD"] != amount("0.25") {
		t.Fatal("failed fallback retained derived usage or lost recorded money")
	}
	turn.Steps[2].Model.Cost = nil
	view, err = Replay(turn)
	if err != nil || view.Spend.CostUnknown != 1 || len(view.Spend.Cost) != 0 {
		t.Fatalf("incomplete fallback was priced: %v", err)
	}
}

func TestReplayDerivedOverflowKeepsPartialMonetaryEvidence(t *testing.T) {
	turn := accountingRecord(llm.Usage{
		Requests: map[string]int{llm.MeterUnreported: math.MaxInt},
	})
	turn.Steps[1].Model.Tools = []tool.Definition{{
		Name: "search",
		Kind: tool.KindProviderTool,
		ProviderTool: &tool.ProviderToolSpec{
			Provider: "test",
			API:      "test.chat",
			Type:     "web_search",
		},
	}}
	turn.Steps[2].Model.Cost.UnpricedRequests = map[string]int{
		llm.MeterUnreported: math.MaxInt,
	}
	turn.Steps[0].Started.Budget = &Budget{
		MaxCost: &Money{Amount: amount("1"), Currency: "USD"},
	}
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Spend.UsageIncomplete || view.Spend.CostPartial != 1 ||
		view.Spend.CostUnknown != 1 ||
		view.Spend.UnpricedRequests[llm.MeterUnreported] != math.MaxInt ||
		view.Spend.Cost["USD"] != amount("0.25") {
		t.Fatal("derivation failure erased independent partial-cost evidence")
	}
}

type creditAdmission struct {
	Account  string       `json:"account"`
	Amount   money.Amount `json:"amount"`
	Currency string       `json:"currency"`
}

type creditRecorder struct {
	mu      sync.Mutex
	base    *MemoryRecorder
	balance money.Amount
	holds   map[string]money.Amount
}

func (r *creditRecorder) Commit(ctx context.Context, req CommitRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.Step.Kind {
	case StepModelCallStarted:
		fact := req.Step.Model
		var admission creditAdmission
		if fact == nil || fact.WorstCaseCost == nil ||
			json.Unmarshal(fact.Reservation["credits"], &admission) != nil ||
			admission.Account != "tenant-1" ||
			admission.Currency != fact.WorstCaseCost.Currency ||
			admission.Amount != fact.WorstCaseCost.Total ||
			admission.Amount > r.balance {
			return &CommitError{
				Persistence: PersistenceRejected,
				Err:         errors.New("credit reservation rejected"),
			}
		}
		if err := r.base.Commit(ctx, req); err != nil {
			return err
		}
		r.balance -= admission.Amount
		r.holds[fact.ModelCallID] = admission.Amount
		return nil
	case StepModelCallFinished:
		fact := req.Step.Model
		if fact != nil {
			if hold, ok := r.holds[fact.ModelCallID]; ok {
				if err := r.base.Commit(ctx, req); err != nil {
					return err
				}
				r.balance += hold
				if fact.Cost != nil {
					r.balance -= fact.Cost.Total
				}
				delete(r.holds, fact.ModelCallID)
				return nil
			}
		}
	}
	return r.base.Commit(ctx, req)
}

// creditHooks admit a call against the recorder's balance, recording the
// reservation terms under the hook name "credits".
func creditHooks(recorder *creditRecorder) Hooks {
	return SendHook{
		Name: "credits",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			if plan.WorstCaseCost == nil {
				return SendDecision{
					Reason: "price unavailable",
					Limit: &LimitDetail{
						Kind: LimitHost,
						Code: "cost_estimate_unavailable",
					},
				}, nil
			}
			recorder.mu.Lock()
			available := recorder.balance
			recorder.mu.Unlock()
			if plan.WorstCaseCost.Total > available {
				return SendDecision{
					Reason: "insufficient credits",
					Limit: &LimitDetail{
						Kind: LimitHost,
						Code: "insufficient_credits",
					},
				}, nil
			}
			data, err := json.Marshal(creditAdmission{
				Account:  "tenant-1",
				Amount:   plan.WorstCaseCost.Total,
				Currency: plan.WorstCaseCost.Currency,
			})
			return SendDecision{Reservation: data}, err
		},
	}.Hooks()
}

func TestBudgetRecorderReservesExactRiskAndSettlesCharge(t *testing.T) {
	answer := final("done")
	answer.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 100}
	client := newPricedClient(answer)
	recorder := &creditRecorder{
		base:    NewMemoryRecorder(),
		balance: amount("0.01"),
		holds:   map[string]money.Amount{},
	}
	def := testAgent()
	def.Settings.MaxTokens = 100
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
		Hooks:    creditHooks(recorder),
	}).Run(
		t.Context(),
		Turn{ID: "credit"},
		Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	intent := out.Turn.Steps[1].Model
	settled := out.Turn.Steps[2].Model
	if intent.WorstCaseCost == nil || settled.Cost == nil {
		t.Fatalf("risk or charge missing: intent=%+v settled=%+v", intent, settled)
	}
	if intent.WorstCaseCost.Total <= settled.Cost.Total {
		t.Fatalf("risk=%s charge=%s", intent.WorstCaseCost.Total, settled.Cost.Total)
	}
	if len(recorder.holds) != 0 {
		t.Fatalf("remaining holds=%v", recorder.holds)
	}
	// Exact amounts make the ledger balance to the nano.
	if recorder.balance != amount("0.01")-settled.Cost.Total {
		t.Fatalf("balance=%s charge=%s", recorder.balance, settled.Cost.Total)
	}
}
