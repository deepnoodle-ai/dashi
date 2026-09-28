package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/money"
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
			json.Unmarshal(fact.Admission, &admission) != nil ||
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

type creditPolicy struct {
	AllowLocalPolicy
	recorder *creditRecorder
}

func (p creditPolicy) BeforeSend(
	_ context.Context,
	plan SendPlan,
) (ModelAuthorization, error) {
	if plan.AtRisk == nil {
		return ModelAuthorization{
			Reason: "price unavailable",
			Limit: &LimitDetail{
				Kind: LimitHost,
				Code: "cost_estimate_unavailable",
			},
		}, nil
	}
	p.recorder.mu.Lock()
	available := p.recorder.balance
	p.recorder.mu.Unlock()
	if plan.AtRisk.Total > available {
		return ModelAuthorization{
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
	return ModelAuthorization{Allow: true, Admission: data}, err
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
	def := definition()
	def.Settings.MaxTokens = 100
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "credit"},
		def,
		Start{
			ID: "start",
			Input: []llm.Message{
				llm.UserMessage(llm.Text("answer")),
			},
		},
		Bindings{
			Client:   client,
			Recorder: recorder,
			Policy:   creditPolicy{recorder: recorder},
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
