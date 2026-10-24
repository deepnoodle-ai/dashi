package main

import (
	"context"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// /task ID continue answers a stopped task's recorded bash approval. In
// bypass mode the command-risk check still reads the command, and a
// flagged command is asked about instead of approved.
func TestRecordedTaskBashApprovalIsCheckedInBypassMode(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true})
	srv.Answer("severe", decidetest.NoulAnswer(0.91))
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Reason: declinedReason}, nil
	}
	c := h.app.ctrl
	pending := dashi.Pending{Kind: dashi.PendingApproval, ID: "c1", Name: "bash", Args: []byte(`{"command":"echo severe"}`)}
	owner := brokerOwner{TaskID: "child", Call: dashi.CallRef{TurnID: "child-1", CallID: pending.ID}}
	ctx, request := c.recordedApproval(context.WithValue(t.Context(), brokerOwnerKey{}, owner), "child-1", pending)
	answer, err := c.Approve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Allow || len(srv.Requests()) != 1 || len(h.ui.approvals) != 1 {
		t.Fatalf("answer=%+v checks=%d approvals=%d", answer, len(srv.Requests()), len(h.ui.approvals))
	}
	if a := h.ui.approvals[0]; !a.Hold || a.Command != "echo severe" {
		t.Fatalf("approval = %+v", a)
	}
}
