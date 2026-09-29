package dashi

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestMalformedInputTurnReturnsErrorWithoutPanic(t *testing.T) {
	turn := Turn{ID: "t1", Steps: []Step{{
		Version: 1,
		TurnID:  "t1",
		Seq:     1,
		Kind:    StepStarted,
		Output:  &StructuredOutput{Value: json.RawMessage(`{bad`)},
	}}}
	out, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(),
		turn,
		Continue{CommandID: "c"},
	)
	if !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("err=%v", err)
	}
	if out.Turn.ID != "t1" || len(out.Turn.Steps) != 1 {
		t.Fatalf("outcome lost the caller's turn: %+v", out.Turn)
	}
}

func hostedRequest(id string) llm.Result {
	request := llm.HostedApprovalRequest{
		ID:     id,
		Name:   "remote",
		Origin: fixtureOrigin,
	}
	return llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:                  llm.ContentHostedApprovalRequest,
			HostedApprovalRequest: &request,
		}),
		Stop: llm.Stop{Kind: llm.StopFinished},
	}
}

// A provider may reuse a hosted request ID after the first request was
// answered. The second request needs its own answer command.
func TestReusedHostedApprovalIDGetsItsOwnAnswer(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		hostedRequest("h1"),
		hostedRequest("h1"),
		final("done"),
	}}
	agent := testAgent()
	agent.Limits.ModelAttempts = 4
	runner := Runner{Client: client}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{CommandID: "start", Agent: agent, Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("start pending=%v err=%v", out.Pending, err)
	}
	first := out.Pending[0]
	if out, err = runner.Run(t.Context(), out.Turn, first.Approve()); err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Pending) != 1 || out.Pending[0].ID != "h1" {
		t.Fatalf("second request pending=%v stop=%+v", out.Pending, out.Stop)
	}
	second := out.Pending[0]
	if second.AttemptID == first.AttemptID ||
		second.Deny("no").CommandID == first.Deny("no").CommandID {
		t.Fatalf("requests share an identity: %+v %+v", first, second)
	}
	if out, err = runner.Run(t.Context(), out.Turn, second.Deny("no")); err != nil {
		t.Fatal(err)
	}
	if len(out.Pending) != 0 || !out.ReadyToContinue {
		t.Fatalf("answered request still pending: %v", out.Pending)
	}
	steps := len(out.Turn.Steps)
	again, err := runner.Run(t.Context(), out.Turn, second.Deny("no"))
	if err != nil || len(again.Turn.Steps) != steps {
		t.Fatalf("retried answer: steps=%d err=%v", len(again.Turn.Steps), err)
	}
	done, err := runner.Run(t.Context(), out.Turn, Continue{CommandID: "c2"})
	if err != nil || done.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", done.Stop, err)
	}
}
