package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type testAdmission struct {
	Units        int    `json:"units"`
	RequestProof string `json:"request_proof"`
}

// budgetHooks admit each call with reservation terms bound to the request,
// recorded under the hook name "budget".
func budgetHooks() Hooks {
	return SendHook{
		Name: "budget",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			admission, err := json.Marshal(testAdmission{
				Units:        3,
				RequestProof: digest(plan.Request.Body()),
			})
			return SendDecision{Admission: admission}, err
		},
	}.Hooks()
}

// This fixture makes record, reservation, usage, and ownership updates one
// serialized operation. A real host must provide the same atomicity in storage.
type budgetRecorder struct {
	mu           sync.Mutex
	base         *MemoryRecorder
	balance      int
	reservations map[string]int
	charged      map[string]bool
	loseIntent   bool
}

func newBudgetRecorder() *budgetRecorder {
	return &budgetRecorder{
		base:         NewMemoryRecorder(),
		balance:      10,
		reservations: map[string]int{},
		charged:      map[string]bool{},
		loseIntent:   true,
	}
}

func (r *budgetRecorder) Claim(ctx context.Context, id, owner string) error {
	return r.base.Claim(ctx, id, owner)
}

func (r *budgetRecorder) Load(ctx context.Context, id string) (Turn, error) {
	return r.base.Load(ctx, id)
}

var _ Loader = (*budgetRecorder)(nil)

func (r *budgetRecorder) Commit(ctx context.Context, req CommitRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	step := req.Step
	var admission testAdmission
	if step.Kind == StepModelIntent {
		if err := json.Unmarshal(
			step.Model.Admission["budget"],
			&admission,
		); err != nil || admission.Units <= 0 || admission.RequestProof != step.Model.RequestDigest {
			return &CommitError{
				Persistence: PersistenceRejected,
				Err:         errors.New("invalid admission"),
			}
		}
		if r.balance < admission.Units {
			return &CommitError{
				Persistence: PersistenceRejected,
				Err:         errors.New("budget exhausted"),
			}
		}
	}
	if err := r.base.Commit(ctx, req); err != nil {
		return err
	}
	if step.Kind == StepModelIntent {
		if _, exists := r.reservations[step.Model.AttemptID]; !exists {
			r.balance -= admission.Units
			r.reservations[step.Model.AttemptID] = admission.Units
		}
		if r.loseIntent {
			r.loseIntent = false
			return errors.New("admission acknowledgment lost after commit")
		}
	}
	if (step.Kind == StepModelSettled || step.Kind == StepModelReconciled) && !r.charged[step.Model.AttemptID] {
		reserved := r.reservations[step.Model.AttemptID]
		charged := reserved
		if step.Model.Failure == llm.FailureNotDispatched || step.Model.Failure == llm.FailureRejected {
			charged = 0
		} else if step.Model.Result != nil && step.Model.Result.Usage != nil {
			charged = step.Model.Result.Usage.InputTokens
		}
		if charged < reserved {
			r.balance += reserved - charged
		}
		r.charged[step.Model.AttemptID] = true
	}
	return nil
}

func TestHostAdmissionTakeoverAndExternalToolRecovery(t *testing.T) {
	// The third and last attempt must complete the turn normally.
	def := testAgent()
	def.Limits.NoReservedFinalAnswer = true
	firstModel := callResult("job", "work")
	firstModel.Usage = &llm.Usage{InputTokens: 2}
	lastModel := final("done")
	lastModel.Usage = &llm.Usage{InputTokens: 1}
	client := &scriptedClient{results: []llm.Result{firstModel, lastModel}}
	rec := newBudgetRecorder()
	rec.Claim(t.Context(), "t1", "owner-a")
	history := []llm.Message{llm.UserMessage(llm.Text("previous context"))}
	startCommand := Start{
		CommandID: "start",
		Agent:     def,
		Origin:    StartScheduled,
		History:   history,
		Input:     []llm.Message{llm.UserMessage(llm.Text("run job"))},
	}
	tools := tool.List{
		tool.Tool{
			Definition: tool.Definition{
				Name:   "work",
				Schema: []byte(`{"type":"object"}`),
				Kind:   tool.KindExternal,
			},
		},
	}
	runner := Runner{
		Client:   client,
		Tools:    tools,
		Recorder: rec,
		Hooks:    budgetHooks(),
	}
	first, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startCommand,
		WithOwner("owner-a"),
	)
	if persistence(err) != PersistenceUnknown || client.sends != 0 || rec.balance != 7 {
		t.Fatalf(
			"lost admission: out=%+v err=%v sends=%d balance=%d",
			first,
			err,
			client.sends,
			rec.balance,
		)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(saved.Steps) != 2 || saved.Steps[1].Model.RequestDigest == "" || len(saved.Steps[1].Model.Admission) == 0 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	view, err := Replay(saved)
	if err != nil || !view.ReadyToContinue {
		t.Fatalf("crashed model intent is not ready: view=%+v err=%v", view, err)
	}
	changed := startCommand
	changed.Agent.Revision = "r2"
	_, err = runner.Run(t.Context(), saved, changed, WithOwner("owner-a"))
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("changed agent accepted: %v", err)
	}
	rec.Claim(t.Context(), "t1", "owner-b")
	proof := Settle{
		CommandID: "no-dispatch",
		CallID:    "t1:model:1",
		ModelFailure: &ModelFailure{
			Disposition: llm.FailureNotDispatched,
			Reason:      "worker did not send",
			Evidence:    "dispatch log cursor 1",
		},
	}
	stale, err := runner.Run(
		t.Context(),
		saved,
		proof,
		WithOwner("owner-a"),
	)
	if persistence(err) != PersistenceRejected || rec.balance != 7 {
		t.Fatalf("stale owner: out=%+v err=%v balance=%d", stale, err, rec.balance)
	}
	proved, err := runner.Run(
		t.Context(),
		saved,
		proof,
		WithOwner("owner-b"),
	)
	if err != nil || !proved.ReadyToContinue || rec.balance != 10 ||
		client.sends != 0 {
		t.Fatalf(
			"no-dispatch reconciliation: out=%+v err=%v balance=%d sends=%d",
			proved,
			err,
			rec.balance,
			client.sends,
		)
	}
	waiting, err := runner.Run(
		t.Context(),
		proved.Turn,
		Continue{CommandID: "retry", History: history},
		WithOwner("owner-b"),
	)
	if err != nil || waiting.Stop.Kind != StopWaiting || client.sends != 1 || rec.balance != 8 {
		t.Fatalf(
			"external wait: out=%+v err=%v sends=%d balance=%d",
			waiting,
			err,
			client.sends,
			rec.balance,
		)
	}
	rec.Claim(t.Context(), "t1", "owner-c")
	result := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("completed in host worker")),
	}
	settle := Settle{CommandID: "external-result", CallID: "job", Outcome: &result}
	stale, err = runner.Run(
		t.Context(),
		waiting.Turn,
		settle,
		WithOwner("owner-b"),
	)
	if persistence(err) != PersistenceRejected || client.sends != 1 {
		t.Fatalf(
			"stale external worker: out=%+v err=%v sends=%d",
			stale,
			err,
			client.sends,
		)
	}
	accepted, err := runner.Run(
		t.Context(),
		waiting.Turn,
		settle,
		WithOwner("owner-c"),
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 1 ||
		rec.balance != 8 {
		t.Fatalf(
			"accepted: out=%+v err=%v sends=%d balance=%d",
			accepted,
			err,
			client.sends,
			rec.balance,
		)
	}
	done, err := runner.Run(
		t.Context(),
		accepted.Turn,
		Continue{CommandID: "finish", History: history},
		WithOwner("owner-c"),
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 2 || rec.balance != 7 || done.Answer != "done" {
		t.Fatalf(
			"done: out=%+v err=%v sends=%d balance=%d",
			done,
			err,
			client.sends,
			rec.balance,
		)
	}
	duplicate, err := runner.Run(
		t.Context(),
		waiting.Turn,
		settle,
		WithOwner("owner-c"),
	)
	if err != nil || duplicate.Stop.Kind != StopCompleted || rec.balance != 7 || client.sends != 2 {
		t.Fatalf(
			"duplicate: out=%+v err=%v sends=%d balance=%d",
			duplicate,
			err,
			client.sends,
			rec.balance,
		)
	}
}
