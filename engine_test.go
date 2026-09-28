package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type scriptedClient struct {
	results []llm.Result
	errors  []error
	deltas  []llm.Delta
	inputs  []llm.Input
	sends   int
}

func (*scriptedClient) Provider() string                       { return "test" }
func (*scriptedClient) API() string                            { return "test.chat" }
func (*scriptedClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }
func (c *scriptedClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	c.inputs = append(c.inputs, in)
	return llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, Input: in, Effective: in.Settings, Body: []byte(`{"ok":true}`)})
}
func (c *scriptedClient) Send(_ context.Context, _ llm.Request, sink llm.Sink) (llm.Result, error) {
	for _, delta := range c.deltas {
		if sink != nil {
			sink(delta)
		}
	}
	if c.sends >= len(c.results) {
		return llm.Result{}, errors.New("unexpected model call")
	}
	result := c.results[c.sends]
	if result.Requested.Provider == "" {
		result.Requested = llm.ModelRef{Provider: "test", Model: "fixture"}
	}
	var err error
	if c.sends < len(c.errors) {
		err = c.errors[c.sends]
	}
	c.sends++
	return result, err
}

type fixedTools []tool.Tool

func (f fixedTools) Tools(context.Context) ([]tool.Tool, error) { return []tool.Tool(f), nil }

type execFunc func(context.Context, tool.Call) (tool.Outcome, error)

func (f execFunc) Execute(ctx context.Context, c tool.Call) (tool.Outcome, error) { return f(ctx, c) }
func definition() Definition {
	return Definition{ID: "agent", Revision: "r1", Provider: "test", API: "test.chat", Model: "fixture", Limits: Limits{ModelAttempts: 3, ProviderPauses: 1, ToolConcurrency: 2}}
}
func final(text string) llm.Result {
	return llm.Result{Message: llm.AssistantMessage(llm.Text(text)), Stop: llm.Stop{Kind: llm.StopFinished}, Requested: llm.ModelRef{Provider: "test", Model: "fixture"}}
}
func callResult(id, name string) llm.Result {
	call := tool.Call{ID: id, Name: name, Args: []byte(`{"value":1}`)}
	return llm.Result{Message: llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}), Stop: llm.Stop{Kind: llm.StopToolUse}, Requested: llm.ModelRef{Provider: "test", Model: "fixture"}}
}
func start(t *testing.T, client *scriptedClient, tools tool.Toolset, rec Recorder, policy Policy) (Outcome, error) {
	t.Helper()
	return (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}}, Bindings{Client: client, Tools: tools, Recorder: rec, Policy: policy})
}
func TestSimpleTurnAndDuplicateStart(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || got.Persistence != PersistenceAcknowledged || client.sends != 1 {
		t.Fatalf("outcome=%+v sends=%d", got.Stop, client.sends)
	}
	view, err := Replay(got.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelAttempts != 1 || len(view.Pending) != 0 {
		t.Fatalf("view=%+v", view)
	}
	again, err := (Engine{}).Apply(context.Background(), got.Turn, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if again.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("duplicate repeated model: %+v", again.Stop)
	}
	stale, err := (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil || stale.Stop.Kind != StopCompleted || client.sends != 1 || len(stale.Turn.Steps) != len(got.Turn.Steps) {
		t.Fatalf("stale duplicate: stop=%+v err=%v sends=%d", stale.Stop, err, client.sends)
	}
}

func TestEmptyTextIsNotCompletion(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("   ")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopFailed || got.Stop.Reason != "empty_response" {
		t.Fatalf("stop=%+v", got.Stop)
	}
}
func TestExternalToolAcceptance(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup"), final("found")}}
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindExternal}}
	got, err := start(t, client, fixedTools{external}, rec, AllowLocalPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopWaiting || client.sends != 1 {
		t.Fatalf("initial stop=%+v sends=%d", got.Stop, client.sends)
	}
	out := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("result"))}
	accepted, err := (Engine{}).Apply(context.Background(), got.Turn, definition(), Accept{ID: "result-1", CallID: "call-1", Outcome: &out}, Bindings{Client: client, Tools: fixedTools{external}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		accepted.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{
			Client:   client,
			Tools:    fixedTools{external},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("completed=%+v err=%v sends=%d", completed, err, client.sends)
	}
	if len(client.inputs[1].Messages) != 3 {
		t.Fatalf("second model messages=%d", len(client.inputs[1].Messages))
	}
	last := client.inputs[1].Messages[2].Content[0].ToolResult
	if last == nil || last.CallID != "call-1" {
		t.Fatalf("missing paired tool result: %+v", last)
	}
	same, err := (Engine{}).Apply(
		context.Background(),
		completed.Turn,
		definition(),
		Accept{ID: "result-1", CallID: "call-1", Outcome: &out},
		Bindings{
			Client:   client,
			Tools:    fixedTools{external},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if same.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("duplicate acceptance sent again")
	}
}

type failIntentRecorder struct{ base *MemoryRecorder }

func (f failIntentRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelIntent {
		return &CommitError{Persistence: PersistenceRejected, Err: errors.New("budget refused")}
	}
	return f.base.Commit(ctx, req)
}

type failOnceKind struct {
	base   *MemoryRecorder
	kind   StepKind
	failed bool
}

func (f *failOnceKind) Commit(ctx context.Context, req CommitRequest) error {
	if !f.failed && req.Step.Kind == f.kind {
		f.failed = true
		return &CommitError{Persistence: PersistenceRejected, Err: errors.New("injected failure")}
	}
	return f.base.Commit(ctx, req)
}

type notifyingRecorder struct {
	Recorder
	onCommit func(Step)
}

func (r notifyingRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if r.onCommit != nil {
		r.onCommit(req.Step)
	}
	return nil
}

type denyPolicy struct{ AllowLocalPolicy }

func (denyPolicy) AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error) {
	return ToolDecision{Kind: DecisionDeny, Reason: "blocked"}, nil
}
func TestDeniedToolRecoveryDoesNotReauthorize(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote"), final("handled")}}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepToolSettled}
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "remote", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		runs++
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("ran"))}, nil
	})}
	got, err := start(t, client, fixedTools{binding}, rec, denyPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Persistence != PersistenceRejected || runs != 0 {
		t.Fatalf("first outcome=%+v runs=%d", got, runs)
	}
	saved, err := base.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := (Engine{}).Apply(context.Background(), saved, definition(), Continue{ID: "recover"}, Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: AllowLocalPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stop.Kind != StopCompleted || runs != 0 || client.sends != 2 {
		t.Fatalf("resumed=%+v runs=%d sends=%d", resumed.Stop, runs, client.sends)
	}
}

type deferredPolicy struct{ AllowLocalPolicy }

func (deferredPolicy) AuthorizeTool(context.Context, tool.Call, TurnView) (ToolDecision, error) {
	return ToolDecision{Kind: DecisionDefer, Revision: "policy-v1"}, nil
}

type rewritePolicy struct{ AllowLocalPolicy }

func (rewritePolicy) AuthorizeTool(_ context.Context, call tool.Call, _ TurnView) (ToolDecision, error) {
	if string(call.Args) == `{"value":1}` {
		return ToolDecision{Rewrite: []byte(`{"value":2}`)}, nil
	}
	if string(call.Args) != `{"value":2}` {
		return ToolDecision{}, errors.New("unexpected effective arguments")
	}
	return ToolDecision{Kind: DecisionAllow, Revision: "rewrite-v1"}, nil
}

func TestToolRewriteRequiresReauthorization(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write"), final("done")}}
	rec := NewMemoryRecorder()
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(_ context.Context, call tool.Call) (tool.Outcome, error) {
		runs++
		if string(call.Args) != `{"value":2}` || string(call.RequestedArgs) != `{"value":1}` {
			return tool.Outcome{}, errors.New("wrong effective tool call")
		}
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("ok"))}, nil
	})}
	got, err := start(t, client, fixedTools{binding}, rec, rewritePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("stop=%+v runs=%d", got.Stop, runs)
	}
	view, err := Replay(got.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Tools["call-1"].Call.Args) != `{"value":2}` || string(view.Tools["call-1"].Call.RequestedArgs) != `{"value":1}` {
		t.Fatalf("recorded call=%+v", view.Tools["call-1"].Call)
	}
}
func TestApprovalBoundToCallAndPolicy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write"), final("written")}}
	rec := NewMemoryRecorder()
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		runs++
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("ok"))}, nil
	})}
	got, err := start(t, client, fixedTools{binding}, rec, deferredPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopWaiting || runs != 0 {
		t.Fatalf("stop=%+v runs=%d", got.Stop, runs)
	}
	bad := ApprovalFact{CallID: "call-1", Approve: true, ArgsDigest: "wrong", PolicyRevision: "policy-v1"}
	_, err = (Engine{}).Apply(context.Background(), got.Turn, definition(), Accept{ID: "bad", CallID: "call-1", Approval: &bad}, Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: deferredPolicy{}})
	if err == nil {
		t.Fatal("stale approval was accepted")
	}
	view, err := Replay(got.Turn)
	if err != nil {
		t.Fatal(err)
	}
	good := ApprovalFact{CallID: "call-1", Approve: true, ArgsDigest: digest(view.Tools["call-1"].Call.Args), PolicyRevision: "policy-v1"}
	accepted, err := (Engine{}).Apply(context.Background(), got.Turn, definition(), Accept{ID: "approve", CallID: "call-1", Approval: &good}, Bindings{Client: client, Tools: fixedTools{binding}, Recorder: rec, Policy: deferredPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.ReadyToContinue || runs != 0 {
		t.Fatalf("accepted=%+v runs=%d", accepted, runs)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		accepted.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{
			Client:   client,
			Tools:    fixedTools{binding},
			Recorder: rec,
			Policy:   deferredPolicy{},
		},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || runs != 1 {
		t.Fatalf("completed=%+v err=%v runs=%d", completed, err, runs)
	}
}

func TestAcceptedModelResultDoesNotSendAgain(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("unneeded")}}
	rec := NewMemoryRecorder()
	def := definition()
	turn := Turn{ID: "t1"}
	commit := func(step Step) {
		t.Helper()
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(context.Background(), CommitRequest{Step: step, ExpectedSeq: step.Seq - 1}); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	commit(Step{Kind: StepStarted, Started: &StartFact{Definition: def, Input: []llm.Message{llm.UserMessage(llm.Text("hello"))}}})
	commit(Step{Kind: StepModelIntent, Model: &ModelFact{AttemptID: "t1:model:1", MessageID: "t1:model:1:message", RequestDigest: "digest", Provider: def.Provider, API: def.API, Model: def.Model}})
	commit(Step{Kind: StepStopped, Stop: &Stop{Kind: StopUncertain, Reason: "worker crashed"}})
	answer := final("recovered")
	got, err := (Engine{}).Apply(context.Background(), turn, def, Accept{ID: "reconcile", CallID: "t1:model:1", ModelResult: &answer}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReadyToContinue || client.sends != 0 {
		t.Fatalf("accepted=%+v sends=%d", got, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		got.Turn,
		def,
		Continue{ID: "continue"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 0 {
		t.Fatalf("completed=%+v err=%v sends=%d", completed, err, client.sends)
	}
}

func TestCommittedFinalResultRecoversWithoutNewModelCall(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("should not send")}}
	rec := NewMemoryRecorder()
	def := definition()
	turn := Turn{ID: "t1"}
	commit := func(step Step) {
		t.Helper()
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(context.Background(), CommitRequest{Step: step, ExpectedSeq: step.Seq - 1}); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	commit(Step{Kind: StepStarted, Started: &StartFact{Definition: def, Input: []llm.Message{llm.UserMessage(llm.Text("hello"))}}})
	commit(Step{Kind: StepModelIntent, Model: &ModelFact{AttemptID: "t1:model:1", MessageID: "t1:model:1:message", RequestDigest: "digest", Provider: def.Provider, API: def.API, Model: def.Model}})
	answer := final("saved")
	commit(Step{Kind: StepModelSettled, Model: &ModelFact{AttemptID: "t1:model:1", MessageID: "t1:model:1:message", Result: &answer}})
	got, err := (Engine{}).Apply(context.Background(), turn, def, Continue{ID: "recover"}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || client.sends != 0 {
		t.Fatalf("outcome=%+v sends=%d", got.Stop, client.sends)
	}
}

func TestUncertainModelNeedsReconciliation(t *testing.T) {
	partial := llm.Result{Usage: &llm.Usage{InputTokens: 3}}
	client := &scriptedClient{results: []llm.Result{partial}, errors: []error{errors.New("connection lost")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopUncertain || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", got.Stop, client.sends)
	}
	_, err = (Engine{}).Apply(context.Background(), got.Turn, definition(), Continue{ID: "retry"}, Bindings{Client: client, Recorder: rec})
	if err == nil {
		t.Fatal("uncertain model attempt was retryable")
	}
	answer := final("recovered")
	answer.Usage = &llm.Usage{InputTokens: 5}
	settled, err := (Engine{}).Apply(context.Background(), got.Turn, definition(), Accept{ID: "reconcile", CallID: "t1:model:1", ModelResult: &answer}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if !settled.ReadyToContinue || client.sends != 1 {
		t.Fatalf("settled=%+v sends=%d", settled, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		settled.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("completed=%+v err=%v sends=%d", completed, err, client.sends)
	}
	view, err := Replay(settled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.InputTokens != 5 {
		t.Fatalf("usage=%+v", view.Spend.Usage)
	}
}

func TestProviderPauseUsesBoundedNextAttempt(t *testing.T) {
	paused := llm.Result{Message: llm.AssistantMessage(llm.Text("working")), Stop: llm.Stop{Kind: llm.StopPause}}
	client := &scriptedClient{results: []llm.Result{paused, final("done")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("stop=%+v sends=%d", got.Stop, client.sends)
	}
	view, err := Replay(got.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ProviderPauses != 1 || view.ModelAttempts != 2 {
		t.Fatalf("view=%+v", view)
	}
}

func TestHostedApprovalWaitsForExplicitDecision(t *testing.T) {
	origin := content.Origin{Provider: "test", API: "test.chat", Model: "fixture", Scope: "fixture"}
	request := llm.HostedApprovalRequest{ID: "approval-1", Name: "remote", Origin: origin}
	first := llm.Result{Requested: llm.ModelRef{Provider: "test", Model: "fixture"}, Message: llm.AssistantMessage(llm.Content{Kind: llm.ContentHostedApprovalRequest, HostedApprovalRequest: &request}), Stop: llm.Stop{Kind: llm.StopFinished}}
	client := &scriptedClient{results: []llm.Result{first, final("approved")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopWaiting || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", got.Stop, client.sends)
	}
	response := llm.HostedApprovalResponse{RequestID: "approval-1", Approve: true, Origin: origin}
	accepted, err := (Engine{}).Apply(context.Background(), got.Turn, definition(), Accept{ID: "approve", CallID: "approval-1", HostedApproval: &response}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("accepted=%+v sends=%d", accepted, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		accepted.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 2 {
		t.Fatalf("completed=%+v err=%v sends=%d", completed, err, client.sends)
	}
}

func TestStreamEventsCarryAttemptIdentity(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("hi")}, deltas: []llm.Delta{{Kind: llm.DeltaText, Text: "hi"}}}
	rec := NewMemoryRecorder()
	events := make(chan Event, 8)
	got, err := (Engine{}).Apply(context.Background(), Turn{ID: "t1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("hi"))}}, Bindings{Client: client, Recorder: rec, Stream: true, Observer: func(e Event) { events <- e }})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || !client.inputs[0].Stream {
		t.Fatalf("stop=%+v stream=%v", got.Stop, client.inputs[0].Stream)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.Delta != nil {
				if event.TurnID != "t1" || event.AttemptID != "t1:model:1" || event.MessageID != "t1:model:1:message" {
					t.Fatalf("event=%+v", event)
				}
				return
			}
		case <-deadline:
			t.Fatal("missing streamed event")
		}
	}
}

func TestInvalidToolArgumentsNeverExecute(t *testing.T) {
	invalid := callResult("call-1", "strict")
	invalid.Message.Content[0].ToolCall.Args = []byte(`{"wrong":1}`)
	client := &scriptedClient{results: []llm.Result{invalid, final("fixed")}}
	rec := NewMemoryRecorder()
	runs := 0
	binding := tool.Tool{Definition: tool.Definition{Name: "strict", Schema: []byte(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}}}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		runs++
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("ran"))}, nil
	})}
	got, err := start(t, client, fixedTools{binding}, rec, AllowLocalPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted || runs != 0 {
		t.Fatalf("stop=%+v runs=%d", got.Stop, runs)
	}
	view, err := Replay(got.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["call-1"].Outcome == nil || view.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("tool=%+v", view.Tools["call-1"])
	}
}

func TestIncompleteModelCallIsNotProjectedAsExecutableCall(t *testing.T) {
	incomplete := callResult("call-1", "lookup")
	incomplete.Stop = llm.Stop{Kind: llm.StopIncomplete}
	incomplete.Message.Content = append([]llm.Content{llm.Text("partial explanation")}, incomplete.Message.Content...)
	client := &scriptedClient{results: []llm.Result{incomplete, final("recovered")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stop.Kind != StopFailed || first.Stop.Reason != "untrusted stop with client calls" {
		t.Fatalf("first stop=%+v", first.Stop)
	}
	second, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Continue{ID: "resume"}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if second.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf("second stop=%+v inputs=%d", second.Stop, len(client.inputs))
	}
	keptText := false
	for _, message := range client.inputs[1].Messages {
		for _, part := range message.Content {
			if part.Kind == llm.ContentBlock && part.Block != nil && part.Block.Text != nil && part.Block.Text.Text == "partial explanation" {
				keptText = true
			}
			if part.Kind == llm.ContentToolCall {
				t.Fatalf("incomplete tool call projected into new model request: %+v", client.inputs[1].Messages)
			}
		}
	}
	if !keptText {
		t.Fatal("partial assistant text was lost from projection")
	}
}

func TestParallelToolsRecordCompletionButProjectCallOrder(t *testing.T) {
	a := tool.Call{ID: "a", Name: "first", Args: []byte(`{}`)}
	b := tool.Call{ID: "b", Name: "second", Args: []byte(`{}`)}
	requested := llm.Result{Message: llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &a}, llm.Content{Kind: llm.ContentToolCall, ToolCall: &b}), Stop: llm.Stop{Kind: llm.StopToolUse}}
	client := &scriptedClient{results: []llm.Result{requested, final("done")}}
	secondSettled := make(chan struct{})
	rec := notifyingRecorder{Recorder: NewMemoryRecorder(), onCommit: func(step Step) {
		if step.Kind == StepToolSettled && step.Tool.Call.ID == "b" {
			close(secondSettled)
		}
	}}
	first := tool.Tool{Definition: tool.Definition{Name: "first", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		select {
		case <-secondSettled:
			return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("first result"))}, nil
		case <-time.After(5 * time.Second):
			return tool.Outcome{}, errors.New("second tool did not settle concurrently")
		}
	})}
	second := tool.Tool{Definition: tool.Definition{Name: "second", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("second result"))}, nil
	})}
	got, err := start(t, client, fixedTools{first, second}, rec, AllowLocalPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", got.Stop)
	}
	var completions []string
	for _, step := range got.Turn.Steps {
		if step.Kind == StepToolSettled {
			completions = append(completions, step.Tool.Call.ID)
		}
	}
	if strings.Join(completions, ",") != "b,a" {
		t.Fatalf("commit order=%v", completions)
	}
	var projected []string
	for _, m := range client.inputs[1].Messages {
		for _, part := range m.Content {
			if part.ToolResult != nil {
				projected = append(projected, part.ToolResult.CallID)
			}
		}
	}
	if strings.Join(projected, ",") != "a,b" {
		t.Fatalf("projection order=%v", projected)
	}
}

func TestStartedToolCanBeReconciledAfterCrash(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	def := definition()
	turn := Turn{ID: "t1"}
	commit := func(step Step) {
		t.Helper()
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(context.Background(), CommitRequest{Step: step, ExpectedSeq: step.Seq - 1}); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	call := tool.Call{ID: "a", Name: "work", Args: []byte(`{}`)}
	definition := tool.Definition{Name: "work", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}
	commit(Step{Kind: StepStarted, Started: &StartFact{Definition: def, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}})
	commit(Step{Kind: StepModelIntent, Model: &ModelFact{AttemptID: "t1:model:1", MessageID: "t1:model:1:message", RequestDigest: "digest", Provider: def.Provider, API: def.API, Model: def.Model, Tools: []tool.Definition{definition}}})
	requested := llm.Result{Message: llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}), Stop: llm.Stop{Kind: llm.StopToolUse}, Requested: llm.ModelRef{Provider: "test", Model: "fixture"}}
	commit(Step{Kind: StepModelSettled, Model: &ModelFact{AttemptID: "t1:model:1", MessageID: "t1:model:1:message", Result: &requested}})
	commit(Step{Kind: StepToolDecision, Tool: &ToolFact{Call: call, Decision: ToolDecision{Kind: DecisionAllow, Revision: "r1"}}})
	commit(Step{Kind: StepToolStarted, Tool: &ToolFact{Call: call}})
	outcome := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("verified"))}
	got, err := (Engine{}).Apply(context.Background(), turn, def, Accept{ID: "reconcile", CallID: "a", Outcome: &outcome}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReadyToContinue || client.sends != 0 {
		t.Fatalf("accepted=%+v sends=%d", got, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		got.Turn,
		def,
		Continue{ID: "continue"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || completed.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("completed=%+v err=%v sends=%d", completed, err, client.sends)
	}
}
func TestIntentFailurePreventsSend(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("should not send")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, failIntentRecorder{rec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Persistence != PersistenceRejected || got.Unacknowledged == nil || got.Unacknowledged.Kind != StepModelIntent || client.sends != 0 {
		t.Fatalf("outcome=%+v sends=%d", got, client.sends)
	}
	saved, err := rec.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 1 {
		t.Fatalf("saved steps=%d", len(saved.Steps))
	}
}
func TestChangedDefinitionAndCommandConflict(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := definition()
	changed.Revision = "r2"
	_, err = (Engine{}).Apply(context.Background(), got.Turn, changed, Continue{ID: "continue"}, Bindings{Client: client, Recorder: rec})
	if !errors.Is(err, ErrDefinitionChanged) {
		t.Fatalf("definition error=%v", err)
	}
	_, err = (Engine{}).Apply(context.Background(), got.Turn, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("different"))}}, Bindings{Client: client, Recorder: rec})
	if !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("command error=%v", err)
	}
}
func TestSnapshotContextReplacesEarlierItem(t *testing.T) {
	t0 := Turn{ID: "t1"}
	rec := NewMemoryRecorder()
	client := &scriptedClient{results: []llm.Result{final("first")}}
	got, err := (Engine{}).Apply(context.Background(), t0, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	// A completed turn cannot accept context; check replacement on a separate
	// record assembled through the same recorder contract.
	_ = got
	turn := Turn{ID: "ctx"}
	add := func(step Step) {
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		turn.Steps = append(turn.Steps, step)
	}
	add(Step{Kind: StepStarted, Started: &StartFact{Definition: definition(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}})
	add(Step{Kind: StepDelivered, Context: &ContextFact{Items: []ContextItem{{Name: "catalog", Origin: "operator", Snapshot: true, Message: llm.UserMessage(llm.Text("old"))}}}})
	add(Step{Kind: StepDelivered, Context: &ContextFact{Items: []ContextItem{{Name: "catalog", Origin: "operator", Snapshot: true, Message: llm.UserMessage(llm.Text("new"))}}}})
	messages, err := Project(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !strings.Contains(messages[1].Content[0].Block.Text.Text, "new") {
		t.Fatalf("projection=%+v", messages)
	}
}

type lostAcknowledgment struct {
	base *MemoryRecorder
	lost bool
}

func (r *lostAcknowledgment) Commit(ctx context.Context, req CommitRequest) error {
	if err := r.base.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == StepModelIntent && !r.lost {
		r.lost = true
		return errors.New("acknowledgment lost after commit")
	}
	return nil
}

func TestUnknownCommitRequiresReloadBeforeContinuation(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("recovered")}}
	base := NewMemoryRecorder()
	rec := &lostAcknowledgment{base: base}
	got, err := start(t, client, nil, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Persistence != PersistenceUnknown || got.Unacknowledged == nil || got.Unacknowledged.Kind != StepModelIntent || client.sends != 0 {
		t.Fatalf("outcome=%+v sends=%d", got, client.sends)
	}
	saved, err := base.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 2 || saved.Steps[1].Kind != StepModelIntent {
		t.Fatalf("saved steps=%+v", saved.Steps)
	}
	view, err := Replay(saved)
	if err != nil || !view.ReadyToContinue {
		t.Fatalf("saved intent is not ready: view=%+v err=%v", view, err)
	}
	recovered, err := (Engine{}).Apply(
		context.Background(),
		saved,
		definition(),
		Continue{ID: "retry"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || recovered.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("recovered=%+v err=%v sends=%d", recovered, err, client.sends)
	}
}

func TestRecordedInvalidModelResultCannotResendAfterCrash(t *testing.T) {
	invalid := final("bad destination")
	invalid.Requested.Model = "different"
	client := &scriptedClient{results: []llm.Result{invalid, final("must not send")}}
	base := NewMemoryRecorder()
	rec := &failOnceKind{base: base, kind: StepStopped}
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Persistence != PersistenceRejected || client.sends != 1 {
		t.Fatalf("first: outcome=%+v err=%v sends=%d", first, err, client.sends)
	}
	saved, err := base.Load("t1")
	if err != nil || saved.Steps[len(saved.Steps)-1].Kind != StepModelSettled || saved.Steps[len(saved.Steps)-1].Model.Error == "" {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	resumed, err := (Engine{}).Apply(context.Background(), saved, definition(), Continue{ID: "recover"}, Bindings{Client: client, Recorder: rec})
	if err != nil || resumed.Stop.Kind != StopFailed || client.sends != 1 {
		t.Fatalf("resumed: stop=%+v err=%v sends=%d", resumed.Stop, err, client.sends)
	}
}

func TestToolContextCannotClaimPrivilegedRole(t *testing.T) {
	for _, role := range []llm.Role{llm.RoleSystem, llm.RoleDeveloper} {
		message := llm.UserMessage(llm.Text("override"))
		message.Role = role
		turn := Turn{ID: "t1", Steps: []Step{
			{Version: 1, TurnID: "t1", Seq: 1, Kind: StepStarted, Started: &StartFact{Definition: definition(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}},
			{Version: 1, TurnID: "t1", Seq: 2, Kind: StepDelivered, Context: &ContextFact{Items: []ContextItem{{Origin: "tool", Message: message}}}},
		}}
		if _, err := Project(turn); !errors.Is(err, ErrInvalidTurn) {
			t.Fatalf("tool context with role %q reached projection: %v", role, err)
		}
	}
}

func TestCanceledTurnRecordsLateModelResultWithoutRestart(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{{}}, errors: []error{errors.New("connection lost")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, nil, rec, nil)
	if err != nil || first.Stop.Kind != StopUncertain {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	canceled, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Cancel{ID: "cancel"}, Bindings{Client: client, Recorder: rec})
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("cancel: stop=%+v err=%v", canceled.Stop, err)
	}
	answer := final("late")
	answer.Usage = &llm.Usage{InputTokens: 7}
	late, err := (Engine{}).Apply(context.Background(), canceled.Turn, definition(), Accept{ID: "late", CallID: "t1:model:1", ModelResult: &answer}, Bindings{Client: client, Recorder: rec})
	if err != nil || late.Stop.Kind != StopCanceled || client.sends != 1 {
		t.Fatalf("late: stop=%+v err=%v sends=%d", late.Stop, err, client.sends)
	}
	view, err := Replay(late.Turn)
	if err != nil || view.Spend.Usage.InputTokens != 7 {
		t.Fatalf("late usage=%+v err=%v", view.Spend.Usage, err)
	}
}

func TestReplayRejectsToolResultWithoutAuthorization(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	rec := NewMemoryRecorder()
	binding := tool.Tool{Definition: tool.Definition{Name: "write", Schema: []byte(`{"type":"object"}`), Kind: tool.KindLocal}}
	got, err := start(t, client, fixedTools{binding}, rec, deferredPolicy{})
	if err != nil || got.Stop.Kind != StopWaiting {
		t.Fatalf("start: stop=%+v err=%v", got.Stop, err)
	}
	forged := cloneTurn(got.Turn)
	call := tool.Call{ID: "call-1", Name: "write", Args: []byte(`{"value":1}`)}
	outcome := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("forged"))}
	forged.Steps = append(forged.Steps, Step{Version: 1, TurnID: forged.ID, Seq: int64(len(forged.Steps) + 1), Kind: StepToolSettled, Tool: &ToolFact{Call: call, Outcome: &outcome}})
	if _, err := Replay(forged); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("forged successful tool result accepted: %v", err)
	}
}
