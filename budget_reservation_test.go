package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
)

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
	case StepModelIntent:
		fact := req.Step.Model
		var admission creditAdmission
		if fact == nil || fact.AtRisk == nil ||
			json.Unmarshal(fact.Admission["credits"], &admission) != nil ||
			admission.Account != "tenant-1" ||
			admission.Currency != fact.AtRisk.Currency ||
			admission.Amount != fact.AtRisk.Total ||
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
		r.holds[fact.AttemptID] = admission.Amount
		return nil
	case StepModelSettled:
		fact := req.Step.Model
		if fact != nil {
			if hold, ok := r.holds[fact.AttemptID]; ok {
				if err := r.base.Commit(ctx, req); err != nil {
					return err
				}
				r.balance += hold
				if fact.Charge != nil {
					r.balance -= fact.Charge.Total
				}
				delete(r.holds, fact.AttemptID)
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
			if plan.AtRisk == nil {
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
			if plan.AtRisk.Total > available {
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
				Amount:   plan.AtRisk.Total,
				Currency: plan.AtRisk.Currency,
			})
			return SendDecision{Admission: data}, err
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
	if intent.AtRisk == nil || settled.Charge == nil {
		t.Fatalf("risk or charge missing: intent=%+v settled=%+v", intent, settled)
	}
	if intent.AtRisk.Total <= settled.Charge.Total {
		t.Fatalf("risk=%s charge=%s", intent.AtRisk.Total, settled.Charge.Total)
	}
	if len(recorder.holds) != 0 {
		t.Fatalf("remaining holds=%v", recorder.holds)
	}
	// Exact amounts make the ledger balance to the nano.
	if recorder.balance != amount("0.01")-settled.Charge.Total {
		t.Fatalf("balance=%s charge=%s", recorder.balance, settled.Charge.Total)
	}
}
