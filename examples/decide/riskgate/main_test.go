package main

import (
	"context"
	"io"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// TestKeyless runs the stand-in scenario: the listing runs unasked, the
// delete waits for the operator and then runs, and the shell call is denied.
func TestKeyless(t *testing.T) {
	out, err := run(context.Background(), false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop = %s, want completed", out.Stop.Kind)
	}
	calls := map[string]dashi.ToolView{}
	for _, v := range out.Tools {
		calls[v.Call.Name] = v
	}
	ran := func(v dashi.ToolView) bool {
		return v.Outcome != nil && v.Outcome.State == tool.StateSucceeded
	}

	if v := calls["list_files"]; v.Verdict.Hook != "" || !ran(v) {
		t.Errorf("list_files: verdict %+v, ran %v; want no opinion, ran", v.Verdict, ran(v))
	}
	if v := calls["delete_path"]; v.Verdict.Kind != dashi.DecisionNeedsApproval ||
		v.Approval == nil || !ran(v) {
		t.Errorf("delete_path: verdict %s, approval %v, ran %v; want held, approved, ran",
			v.Verdict.Kind, v.Approval, ran(v))
	}
	if v := calls["run_shell"]; v.Verdict.Kind != dashi.DecisionDeny || ran(v) {
		t.Errorf("run_shell: verdict %s, ran %v; want denied, not run", v.Verdict.Kind, ran(v))
	}
}

// TestJudgeFailureHolds checks that the gate fails closed to a person: with
// the decision model unreachable, a call is held for approval.
func TestJudgeFailureHolds(t *testing.T) {
	srv := decidetest.NewServer(t)
	judge := srv.Client(t, decide.WithMaxRetries(0))
	srv.Close()

	hook := riskGate(judge, request, nil)
	d, err := hook.Func(context.Background(), dashi.ToolPlan{
		Call: tool.Call{ID: "call-1", Name: "list_files", Args: []byte(`{"path":"/tmp"}`)},
	})
	if err != nil || d.Kind != dashi.DecisionNeedsApproval {
		t.Fatalf("decision %+v, error %v; want needs_approval", d, err)
	}
}
