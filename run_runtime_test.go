package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestContextInterruptLeavesToolBatchPending(t *testing.T) {
	a := tool.Call{ID: "a", Name: "first", Args: []byte(`{}`)}
	b := tool.Call{ID: "b", Name: "second", Args: []byte(`{}`)}
	client := &scriptedClient{
		results: []llm.Result{
			{
				Message: llm.AssistantMessage(
					llm.Content{Kind: llm.ContentToolCall, ToolCall: &a},
					llm.Content{Kind: llm.ContentToolCall, ToolCall: &b},
				),
				Stop: llm.Stop{Kind: llm.StopToolUse},
			},
		},
	}
	rec := NewMemoryRecorder()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	first := tool.Tool{
		Definition: tool.Definition{
			Name:   "first",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			close(started)
			<-release // Deliberately ignores cancellation.
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("late")),
			}, nil
		}),
	}
	secondRuns := 0
	second := tool.Tool{
		Definition: tool.Definition{
			Name:   "second",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			secondRuns++
			return tool.Outcome{State: tool.StateSucceeded}, nil
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	def := testAgent()
	def.Limits.ToolConcurrency = 1
	finished := make(chan RunResult, 1)
	go func() {
		out, _ := (Runner{
			Client:   client,
			Tools:    tool.List{first, second},
			Recorder: rec,
		}).Run(
			ctx,
			Turn{ID: "t1"},
			Start{CommandID: "start", Agent: def, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
		)
		finished <- out
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first executor did not start")
	}
	cancel()
	var out RunResult
	select {
	case out = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for an executor that ignored cancellation")
	}
	if out.Stop.Kind != StopInterrupted || secondRuns != 0 {
		t.Fatalf("stop=%+v second runs=%d", out.Stop, secondRuns)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["a"].Status != ToolStarted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if status := view.Tools["b"].Status; status != ToolRequested &&
		status != ToolAllowed {
		t.Fatalf("unstarted call status=%s, want pending", status)
	}
	// Only the started call needs the host; the unstarted one needs only
	// Continue.
	if ids := pendingIDs(out.Pending); len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("pending=%v", ids)
	}
	late := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("late")),
	}
	resolved, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		out.Turn,
		Resolve{CommandID: "late", CallID: "a", Outcome: &late},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Stop.Kind != StopInterrupted || len(resolved.Pending) != 0 ||
		!resolved.ReadyToContinue {
		t.Fatalf("late=%+v err=%v", resolved, err)
	}
}

func TestInterruptedParallelBatchDoesNotStartLaterSibling(t *testing.T) {
	parts := []llm.Content{}
	for _, name := range []string{"first", "second", "third"} {
		call := tool.Call{ID: name, Name: name, Args: []byte(`{}`)}
		parts = append(
			parts,
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
		)
	}
	client := &scriptedClient{
		results: []llm.Result{
			{
				Message: llm.AssistantMessage(parts...),
				Stop:    llm.Stop{Kind: llm.StopToolUse},
			},
		},
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	makeTool := func(name string) tool.Tool {
		return tool.Tool{
			Definition: tool.Definition{
				Name:   name,
				Schema: []byte(`{"type":"object"}`),
				Kind:   tool.KindLocal,
			},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				started <- name
				<-release
				return tool.Outcome{State: tool.StateSucceeded}, nil
			}),
		}
	}
	var thirdRuns atomic.Int32
	third := tool.Tool{
		Definition: tool.Definition{
			Name:   "third",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			thirdRuns.Add(1)
			return tool.Outcome{State: tool.StateSucceeded}, nil
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan RunResult, 1)
	rec := NewMemoryRecorder()
	go func() {
		out, _ := (Runner{
			Client:   client,
			Tools:    tool.List{makeTool("first"), makeTool("second"), third},
			Recorder: rec,
		}).Run(
			ctx,
			Turn{ID: "t1"},
			Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
		)
		finished <- out
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel tools did not both start")
		}
	}
	cancel()
	var out RunResult
	select {
	case out = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for the parallel executors")
	}
	if out.Stop.Kind != StopInterrupted || thirdRuns.Load() != 0 ||
		len(out.Pending) != 2 {
		t.Fatalf("out=%+v third runs=%d", out, thirdRuns.Load())
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["first"].Status != ToolStarted ||
		view.Tools["second"].Status != ToolStarted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if status := view.Tools["third"].Status; status != ToolRequested &&
		status != ToolAllowed {
		t.Fatalf("unstarted call status=%s, want pending", status)
	}
}

func TestExplicitCancelFinishesUnstartedApproval(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "write")},
	}
	rec := NewMemoryRecorder()
	first, err := start(
		t,
		client,
		tool.List{
			tool.Tool{
				Definition: tool.Definition{
					Name:   "write",
					Schema: []byte(`{"type":"object"}`),
					Kind:   tool.KindLocal,
				},
			},
		},
		rec,
		deferredHooks(),
	)
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", first, err)
	}
	canceled, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil || canceled.Stop.Kind != StopCanceled || len(canceled.Pending) != 0 {
		t.Fatalf("cancel=%+v err=%v", canceled, err)
	}
	view, err := canceled.Turn.View()
	if err != nil || view.Tools["call-1"].Status != ToolFinished || view.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestInterruptedAfterModelCallStartDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &scriptedClient{results: []llm.Result{final("must not send")}}
	rec := notifyingRecorder{
		Recorder: NewMemoryRecorder(),
		onCommit: func(step Step) {
			if step.Kind == StepModelCallStarted {
				cancel()
			}
		},
	}
	out, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || out.Stop.Kind != StopInterrupted ||
		client.sends != 0 || len(out.Pending) != 0 {
		t.Fatalf("out=%+v err=%v sends=%d", out, err, client.sends)
	}
	callFinished := out.Turn.Steps[len(out.Turn.Steps)-2]
	if callFinished.Kind != StepModelCallFinished ||
		callFinished.Model.Interrupted != InterruptRun {
		t.Fatalf("finish=%+v", callFinished)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelCalls != 0 {
		t.Fatalf("model calls=%d, want 0", view.ModelCalls)
	}
}

func TestInterruptedAfterModelResultDoesNotStartPendingTool(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "work")}}
	rec := notifyingRecorder{
		Recorder: NewMemoryRecorder(),
		onCommit: func(step Step) {
			if step.Kind == StepModelCallFinished {
				cancel()
			}
		},
	}
	runs := 0
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "work",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			runs++
			return tool.Outcome{State: tool.StateSucceeded}, nil
		}),
	}
	out, err := (Runner{
		Client:   client,
		Tools:    tool.List{binding},
		Recorder: rec,
	}).Run(
		ctx,
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || out.Stop.Kind != StopInterrupted || runs != 0 ||
		client.sends != 1 || len(out.Pending) != 0 {
		t.Fatalf("out=%+v err=%v runs=%d sends=%d", out, err, runs, client.sends)
	}
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Status != ToolRequested {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

type testReservation struct {
	Units        int    `json:"units"`
	RequestProof string `json:"request_proof"`
}

// budgetHooks reserve each call with reservation terms bound to the request,
// recorded under the hook name "budget".
func budgetHooks() Hooks {
	return SendHook{
		Name: "budget",
		Func: func(_ context.Context, plan SendPlan) (SendDecision, error) {
			admission, err := json.Marshal(testReservation{
				Units:        3,
				RequestProof: canonjson.Hash(plan.Request.Body()),
			})
			return SendDecision{Reservation: admission}, err
		},
	}.Hooks()
}

// This fixture makes record, reservation, usage, and ownership updates one
// serialized operation. A real host must provide the same atomicity in storage.
type budgetRecorder struct {
	mu            sync.Mutex
	base          *MemoryRecorder
	balance       int
	reservations  map[string]int
	charged       map[string]bool
	loseCallStart bool
}

func newBudgetRecorder() *budgetRecorder {
	return &budgetRecorder{
		base:          NewMemoryRecorder(),
		balance:       10,
		reservations:  map[string]int{},
		charged:       map[string]bool{},
		loseCallStart: true,
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
	var admission testReservation
	if step.Kind == StepModelCallStarted {
		if err := json.Unmarshal(
			step.Model.Reservation["budget"],
			&admission,
		); err != nil || admission.Units <= 0 || admission.RequestProof != step.Model.RequestHash {
			return &CommitError{
				NothingWritten: true,
				Err:            errors.New("invalid reservation"),
			}
		}
		if r.balance < admission.Units {
			return &CommitError{
				NothingWritten: true,
				Err:            errors.New("budget exhausted"),
			}
		}
	}
	if err := r.base.Commit(ctx, req); err != nil {
		return err
	}
	if step.Kind == StepModelCallStarted {
		if _, exists := r.reservations[step.Model.ModelCallID]; !exists {
			r.balance -= admission.Units
			r.reservations[step.Model.ModelCallID] = admission.Units
		}
		if r.loseCallStart {
			r.loseCallStart = false
			return errors.New("reservation acknowledgment lost after commit")
		}
	}
	if (step.Kind == StepModelCallFinished || step.Kind == StepModelCallFinishedByHost) && !r.charged[step.Model.ModelCallID] {
		reserved := r.reservations[step.Model.ModelCallID]
		charged := reserved
		if step.Model.SendStatus == llm.SendStatusNotSent || step.Model.SendStatus == llm.SendStatusProviderRejected {
			charged = 0
		} else if step.Model.Result != nil && step.Model.Result.Usage != nil {
			charged = step.Model.Result.Usage.InputTokens
		}
		if charged < reserved {
			r.balance += reserved - charged
		}
		r.charged[step.Model.ModelCallID] = true
	}
	return nil
}

func TestHostReservationTakeoverAndExternalToolRecovery(t *testing.T) {
	// The third and last model call must complete the turn normally.
	def := testAgent()
	def.Limits.NoReservedFinalReply = true
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
	if !writeUnknown(err) || client.sends != 0 || rec.balance != 7 {
		t.Fatalf(
			"lost reservation: out=%+v err=%v sends=%d balance=%d",
			first,
			err,
			client.sends,
			rec.balance,
		)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || len(saved.Steps) != 2 || saved.Steps[1].Model.RequestHash == "" || len(saved.Steps[1].Model.Reservation) == 0 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	view, err := saved.View()
	if err != nil || !view.ReadyToContinue {
		t.Fatalf("crashed model call start is not ready: view=%+v err=%v", view, err)
	}
	changed := startCommand
	changed.Agent.Version = "r2"
	_, err = runner.Run(t.Context(), saved, changed, WithOwner("owner-a"))
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("changed agent accepted: %v", err)
	}
	rec.Claim(t.Context(), "t1", "owner-b")
	proof := Resolve{
		CommandID: "no-dispatch",
		CallID:    "t1:model:1",
		ModelFailure: &ModelFailure{
			SendStatus: llm.SendStatusNotSent,
			Reason:     "worker did not send",
			Evidence:   "dispatch log cursor 1",
		},
	}
	stale, err := runner.Run(
		t.Context(),
		saved,
		proof,
		WithOwner("owner-a"),
	)
	if !nothingWritten(err) || rec.balance != 7 {
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
			"no-dispatch resolution: out=%+v err=%v balance=%d sends=%d",
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
	finish := Resolve{CommandID: "external-result", CallID: "job", Outcome: &result}
	stale, err = runner.Run(
		t.Context(),
		waiting.Turn,
		finish,
		WithOwner("owner-b"),
	)
	if !nothingWritten(err) || client.sends != 1 {
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
		finish,
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
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 2 || rec.balance != 7 || done.Reply != "done" {
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
		finish,
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
