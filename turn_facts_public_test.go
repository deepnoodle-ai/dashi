package dashi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// These tests approve a tool call using only the public API, with a recorder
// that changes stored bytes the way a Postgres jsonb column does.

// Arguments as a model might write them: jsonb would reorder the keys, drop
// the spaces, decode the escape, and write 1e2 as 100.
var shownArgs = json.RawMessage(`{"b": 1e2, "a": "café"}`)

type approvalClient struct{ sends int }

func (*approvalClient) Provider() string {
	return "test"
}

func (*approvalClient) API() string {
	return "test.chat"
}

func (*approvalClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (*approvalClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
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
			Message: llm.AssistantMessage(llm.Content{
				Kind:     llm.ContentToolCall,
				ToolCall: &call,
			}),
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

// approvalHooks send every tool call to a person for approval.
var approvalHooks = dashi.RequireApproval()

// jsonbStore wraps MemoryRecorder; Load returns each step re-encoded with
// sorted keys, decoded escapes, and exponent numbers expanded.
type jsonbStore struct{ *dashi.MemoryRecorder }

var _ dashi.Loader = jsonbStore{}

func (s jsonbStore) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := s.MemoryRecorder.Load(ctx, id)
	if err != nil {
		return dashi.Turn{}, err
	}
	for i, step := range turn.Steps {
		raw, err := json.Marshal(step)
		if err != nil {
			return dashi.Turn{}, err
		}
		raw = bytes.ReplaceAll(raw, []byte(`1e2`), []byte(`100`))
		var value any // Decoding into any sorts keys and drops escapes.
		if err := json.Unmarshal(raw, &value); err != nil {
			return dashi.Turn{}, err
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(value); err != nil {
			return dashi.Turn{}, err
		}
		var out dashi.Step
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			return dashi.Turn{}, err
		}
		turn.Steps[i] = out
	}
	return turn, nil
}

func pendingApproval(
	t *testing.T,
) (dashi.Turn, dashi.Runner, *approvalClient, *int) {
	t.Helper()
	def := dashi.Agent{
		ID:       "agent",
		Version:  "r1",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
		Limits:   dashi.Limits{ModelCalls: 3, ToolConcurrency: 1},
	}
	runs := new(int)
	write := tool.Tool{
		Definition: tool.Definition{
			Name:   "write",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: toolFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			*runs++
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("ok")),
			}, nil
		}),
	}
	client := &approvalClient{}
	store := jsonbStore{dashi.NewMemoryRecorder()}
	runner := dashi.Runner{
		Client:   client,
		Tools:    tool.List{write},
		Recorder: store,
		Hooks:    approvalHooks,
	}
	out, err := runner.Run(
		t.Context(),
		dashi.Turn{ID: "t1"},
		dashi.Start{
			CommandID: "start",
			Agent:     def,
			Input:     []llm.Message{llm.UserMessage(llm.Text("write it"))},
		},
	)
	if err != nil || out.Stop.Kind != dashi.StopWaiting {
		t.Fatalf("start=%+v err=%v", out.Stop, err)
	}
	// A host with durable storage reloads the turn before resolving.
	turn, err := store.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Tools["call-1"].Call.Args) != `{"a":"café","b":100}` {
		t.Fatalf("store kept argument bytes %s", view.Tools["call-1"].Call.Args)
	}
	return turn, runner, client, runs
}

func TestApprovalHashFromPublicView(t *testing.T) {
	turn, runner, _, runs := pendingApproval(t)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	pending := view.Tools["call-1"]
	if pending.Status != dashi.ToolApprovalPending || pending.ArgsHash == "" {
		t.Fatalf("pending call=%+v", pending)
	}
	if pending.ArgsHash != dashi.ArgsHash(shownArgs) {
		t.Fatal("view hash differs from the hash of the arguments first shown")
	}
	approval := dashi.ApprovalDecision{
		CallID:   "call-1",
		Approve:  true,
		ArgsHash: pending.ArgsHash,
	}
	accepted, err := runner.Run(
		t.Context(),
		turn,
		dashi.Resolve{CommandID: "approve", CallID: "call-1", Approval: &approval},
	)
	if err != nil || !accepted.ReadyToContinue {
		t.Fatalf("approve=%+v err=%v", accepted, err)
	}
	view, err = accepted.Turn.View()
	if err != nil || view.Tools["call-1"].ArgsHash != "" {
		t.Fatalf(
			"answered call keeps its approval hash: %+v err=%v",
			view.Tools["call-1"],
			err,
		)
	}
	done, err := runner.Run(
		t.Context(),
		accepted.Turn,
		dashi.Continue{CommandID: "continue"},
	)
	if err != nil || done.Stop.Kind != dashi.StopCompleted || *runs != 1 {
		t.Fatalf("continue=%+v err=%v runs=%d", done.Stop, err, *runs)
	}
}

func TestApprovalHashFromShownArguments(t *testing.T) {
	turn, runner, _, _ := pendingApproval(t)
	// The UI saw the arguments before the store normalized them.
	approval := dashi.ApprovalDecision{
		CallID:   "call-1",
		Approve:  true,
		ArgsHash: dashi.ArgsHash(shownArgs),
	}
	accepted, err := runner.Run(
		t.Context(),
		turn,
		dashi.Resolve{CommandID: "approve", CallID: "call-1", Approval: &approval},
	)
	if err != nil || !accepted.ReadyToContinue {
		t.Fatalf("approve=%+v err=%v", accepted, err)
	}
}

func TestApprovalOfChangedArgumentsIsStale(t *testing.T) {
	turn, runner, _, runs := pendingApproval(t)
	changed := json.RawMessage(`{"b": 101, "a": "café"}`)
	approval := dashi.ApprovalDecision{
		CallID:   "call-1",
		Approve:  true,
		ArgsHash: dashi.ArgsHash(changed),
	}
	_, err := runner.Run(
		t.Context(),
		turn,
		dashi.Resolve{CommandID: "approve", CallID: "call-1", Approval: &approval},
	)
	if err == nil || !strings.Contains(
		err.Error(),
		"stale tool approval",
	) || *runs != 0 {
		t.Fatalf("approval of different arguments: err=%v runs=%d", err, *runs)
	}
}

// A call that leaves approval without a decision, here by cancellation, no
// longer carries an approval hash.
func TestCanceledApprovalHasNoArgsHash(t *testing.T) {
	turn, runner, _, runs := pendingApproval(t)
	canceled, err := runner.Run(
		t.Context(),
		turn,
		dashi.Cancel{CommandID: "cancel", Reason: "user left"},
	)
	if err != nil || canceled.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	view, err := canceled.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	call := view.Tools["call-1"]
	if call.Status == dashi.ToolApprovalPending || call.ArgsHash != "" || *runs != 0 {
		t.Fatalf("canceled call=%+v runs=%d", call, *runs)
	}
}

type toolFunc func(context.Context, tool.Call) (tool.Outcome, error)

func (f toolFunc) Execute(
	ctx context.Context,
	c tool.Call,
) (tool.Outcome, error) {
	return f(ctx, c)
}
