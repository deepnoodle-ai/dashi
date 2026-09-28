package kernel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

// These tests approve a tool call using only the public API, with a recorder
// that changes stored bytes the way a Postgres jsonb column does.

// Arguments as a model might write them: jsonb would reorder the keys, drop
// the spaces, decode the escape, and write 1e2 as 100.
var shownArgs = json.RawMessage(`{"b": 1e2, "a": "café"}`)

type approvalClient struct{ sends int }

func (*approvalClient) Provider() string                       { return "test" }
func (*approvalClient) API() string                            { return "test.chat" }
func (*approvalClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }
func (*approvalClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin:    content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}
func (c *approvalClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.sends++
	requested := llm.ModelRef{Provider: "test", Model: "fixture"}
	if c.sends == 1 {
		call := tool.Call{ID: "call-1", Name: "write", Args: shownArgs}
		return llm.Result{
			Message:   llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &call}),
			Stop:      llm.Stop{Kind: llm.StopToolUse},
			Requested: requested,
		}, nil
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text("written")),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: requested,
	}, nil
}

// approvalPolicy defers every tool call to a person.
type approvalPolicy struct{ kernel.AllowLocalPolicy }

func (approvalPolicy) AuthorizeTool(context.Context, tool.Call, kernel.TurnView) (kernel.ToolDecision, error) {
	return kernel.ToolDecision{Kind: kernel.DecisionDefer, Revision: "policy-v1"}, nil
}

// jsonbStore wraps MemoryRecorder; Load returns each step re-encoded with
// sorted keys, decoded escapes, and exponent numbers expanded.
type jsonbStore struct{ *kernel.MemoryRecorder }

func (s jsonbStore) Load(id string) (kernel.Turn, error) {
	turn, err := s.MemoryRecorder.Load(id)
	if err != nil {
		return kernel.Turn{}, err
	}
	for i, step := range turn.Steps {
		raw, err := json.Marshal(step)
		if err != nil {
			return kernel.Turn{}, err
		}
		raw = bytes.ReplaceAll(raw, []byte(`1e2`), []byte(`100`))
		var value any // Decoding into any sorts keys and drops escapes.
		if err := json.Unmarshal(raw, &value); err != nil {
			return kernel.Turn{}, err
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(value); err != nil {
			return kernel.Turn{}, err
		}
		var out kernel.Step
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			return kernel.Turn{}, err
		}
		turn.Steps[i] = out
	}
	return turn, nil
}

func pendingApproval(t *testing.T) (kernel.Turn, kernel.Definition, kernel.Bindings, *approvalClient, *int) {
	t.Helper()
	def := kernel.Definition{
		ID: "agent", Revision: "r1", Provider: "test", API: "test.chat", Model: "fixture",
		Limits: kernel.Limits{ModelAttempts: 3, ToolConcurrency: 1},
	}
	runs := new(int)
	write := tool.Tool{
		Definition: tool.Definition{Name: "write", Schema: json.RawMessage(`{"type":"object"}`), Kind: tool.KindLocal},
		Executor: toolFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			*runs++
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	client := &approvalClient{}
	store := jsonbStore{kernel.NewMemoryRecorder()}
	bindings := kernel.Bindings{Client: client, Tools: tools{write}, Recorder: store, Policy: approvalPolicy{}}
	out, err := (kernel.Engine{}).Apply(t.Context(), kernel.Turn{ID: "t1"}, def,
		kernel.Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("write it"))}}, bindings)
	if err != nil || out.Stop.Kind != kernel.StopWaiting {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	// A host with durable storage reloads the turn before answering.
	turn, err := store.Load("t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := kernel.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Tools["call-1"].Call.Args) != `{"a":"café","b":100}` {
		t.Fatalf("store kept argument bytes %s", view.Tools["call-1"].Call.Args)
	}
	return turn, def, bindings, client, runs
}

func TestApprovalDigestFromPublicView(t *testing.T) {
	turn, def, bindings, _, runs := pendingApproval(t)
	view, err := kernel.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	pending := view.Tools["call-1"]
	if pending.Status != kernel.ToolApprovalPending || pending.ArgsDigest == "" {
		t.Fatalf("pending call=%+v", pending)
	}
	if pending.ArgsDigest != kernel.ArgsDigest(shownArgs) {
		t.Fatal("view digest differs from the digest of the arguments first shown")
	}
	approval := kernel.ApprovalFact{CallID: "call-1", Approve: true, ArgsDigest: pending.ArgsDigest, PolicyRevision: pending.Decision.Revision}
	accepted, err := (kernel.Engine{}).Apply(t.Context(), turn, def, kernel.Accept{ID: "approve", CallID: "call-1", Approval: &approval}, bindings)
	if err != nil || !accepted.ReadyToContinue {
		t.Fatalf("approve=%+v err=%v", accepted, err)
	}
	view, err = kernel.Replay(accepted.Turn)
	if err != nil || view.Tools["call-1"].ArgsDigest != "" {
		t.Fatalf("answered call keeps its approval digest: %+v err=%v", view.Tools["call-1"], err)
	}
	done, err := (kernel.Engine{}).Apply(t.Context(), accepted.Turn, def, kernel.Continue{ID: "continue"}, bindings)
	if err != nil || done.Stop.Kind != kernel.StopCompleted || *runs != 1 {
		t.Fatalf("continue=%+v err=%v runs=%d", done.Stop, err, *runs)
	}
}

func TestApprovalDigestFromShownArguments(t *testing.T) {
	turn, def, bindings, _, _ := pendingApproval(t)
	// The UI saw the arguments before the store normalized them.
	approval := kernel.ApprovalFact{CallID: "call-1", Approve: true, ArgsDigest: kernel.ArgsDigest(shownArgs), PolicyRevision: "policy-v1"}
	accepted, err := (kernel.Engine{}).Apply(t.Context(), turn, def, kernel.Accept{ID: "approve", CallID: "call-1", Approval: &approval}, bindings)
	if err != nil || !accepted.ReadyToContinue {
		t.Fatalf("approve=%+v err=%v", accepted, err)
	}
}

func TestApprovalOfChangedArgumentsIsStale(t *testing.T) {
	turn, def, bindings, _, runs := pendingApproval(t)
	changed := json.RawMessage(`{"b": 101, "a": "café"}`)
	approval := kernel.ApprovalFact{CallID: "call-1", Approve: true, ArgsDigest: kernel.ArgsDigest(changed), PolicyRevision: "policy-v1"}
	_, err := (kernel.Engine{}).Apply(t.Context(), turn, def, kernel.Accept{ID: "approve", CallID: "call-1", Approval: &approval}, bindings)
	if err == nil || !strings.Contains(err.Error(), "stale tool approval") || *runs != 0 {
		t.Fatalf("approval of different arguments: err=%v runs=%d", err, *runs)
	}
}

// A call that leaves approval without an answer, here by cancellation, no
// longer carries an approval digest.
func TestCanceledApprovalHasNoArgsDigest(t *testing.T) {
	turn, def, bindings, _, runs := pendingApproval(t)
	canceled, err := (kernel.Engine{}).Apply(t.Context(), turn, def, kernel.Cancel{ID: "cancel", Reason: "user left"}, bindings)
	if err != nil || canceled.Stop.Kind != kernel.StopCanceled {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	view, err := kernel.Replay(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	call := view.Tools["call-1"]
	if call.Status == kernel.ToolApprovalPending || call.ArgsDigest != "" || *runs != 0 {
		t.Fatalf("canceled call=%+v runs=%d", call, *runs)
	}
}

type tools []tool.Tool

func (ts tools) Tools(context.Context) ([]tool.Tool, error) { return ts, nil }

type toolFunc func(context.Context, tool.Call) (tool.Outcome, error)

func (f toolFunc) Execute(ctx context.Context, c tool.Call) (tool.Outcome, error) { return f(ctx, c) }
