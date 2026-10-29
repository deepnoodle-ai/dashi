package main

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
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
	// The denial keeps the check in its rules, never in its reason.
	d := recordedResponse(ctx, pending, answer).Approval
	if d == nil || d.Approve || d.Reason != declinedReason || !strings.HasPrefix(d.RulesVersion, "decide/") ||
		!strings.HasPrefix(d.RulesFinding, "severe 91% flagged (") {
		t.Fatalf("decision = %+v", d)
	}
}

// In default mode, an approved recorded bash call keeps the answer the
// prompt showed. A call decide does not read records no rules.
func TestRecordedTaskApprovalKeepsDecideAnswer(t *testing.T) {
	h, srv := decideHarness(t, Options{})
	srv.Answer("severe", decidetest.NoulAnswer(0.12))
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		if check := h.ui.lastApproval().Check; check != nil {
			<-check.done
		}
		return Approval{Allow: true}, nil
	}
	c := h.app.ctrl
	for _, tc := range []struct {
		pending dashi.Pending
		version string
		finding string
	}{
		{dashi.Pending{Kind: dashi.PendingApproval, ID: "c1", Name: "bash", Args: []byte(`{"command":"ls"}`)},
			"decide/", "severe 12% ("},
		{dashi.Pending{Kind: dashi.PendingApproval, ID: "c2", Name: "write_file", Args: []byte(`{"path":"a"}`)},
			"", ""},
	} {
		owner := brokerOwner{TaskID: "child", Call: dashi.CallRef{TurnID: "child-1", CallID: tc.pending.ID}}
		ctx, request := c.recordedApproval(context.WithValue(t.Context(), brokerOwnerKey{}, owner), "child-1", tc.pending)
		answer, err := c.Approve(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		d := recordedResponse(ctx, tc.pending, answer).Approval
		if d == nil || !d.Approve || d.Reason != "" || !strings.HasPrefix(d.RulesVersion, tc.version) ||
			!strings.HasPrefix(d.RulesFinding, tc.finding) || (tc.version == "") != (d.RulesVersion == "") {
			t.Fatalf("%s: decision = %+v", tc.pending.Name, d)
		}
	}
}

// /task ID shows each recorded approval decision with the decide answer
// that informed it.
func TestTaskDetailsShowRecordedApprovals(t *testing.T) {
	view := dashi.TurnView{ID: "child", Tools: map[string]dashi.ToolView{
		"c1": {Call: tool.Call{ID: "c1", Name: "bash"}, Status: dashi.ToolDenied, Approval: &dashi.ApprovalDecision{
			CallID: "c1", Reason: "no", RulesVersion: "decide/jev-1.13.0", RulesFinding: "severe 91% flagged (jev-1.13.0, request r_12)",
		}},
		"c2": {Call: tool.Call{ID: "c2", Name: "write_file"}, Status: dashi.ToolAllowed, Approval: &dashi.ApprovalDecision{CallID: "c2", Approve: true}},
	}}
	details := taskDetails(taskSnapshot{Bound: true, View: view})
	mustHave(t, details, "Recorded approval: call c1 · bash · denied · decide command: severe 91% — flagged\n")
	mustHave(t, details, "Recorded approval: call c2 · write_file · approved\n")
}
