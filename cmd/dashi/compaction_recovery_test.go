package main

import (
	"testing"

	"github.com/deepnoodle-ai/dashi"
)

// A Turn that stopped for context lets a new request through unless work is
// still pending.
func TestAutomationNewRequestAfterContextStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		child   dashi.Next
		missing bool
		kind    dashi.StopKind
		reason  string
		blocked bool
	}{
		{"summary failed", false, "", false, dashi.StopFailed, dashi.StopReasonCompactionFailed, false},
		{"context window", false, "", false, dashi.StopLimited, string(dashi.LimitContextWindow), false},
		{"pending tool", true, "", false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"pending child", false, dashi.NextResolvePending, false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"unknown child", false, dashi.NextRecover, false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"missing notice", false, "", true, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"other failure", false, "", false, dashi.StopFailed, "provider failed", true},
		{"running", false, "", false, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := dashi.TurnView{ID: "parent", LastSeq: 1, Stop: dashi.Stop{Kind: tc.kind, Reason: tc.reason}}
			if tc.pending {
				parent.Pending = []dashi.Pending{{ID: "unsafe", Kind: dashi.PendingTool}}
			}
			snap := automationSnapshot{latest: parent, missingNotice: tc.missing}
			if tc.child != "" {
				snap.affected = []automationAttention{{TaskID: "child", Next: tc.child}}
			}
			if got := snap.blocksNew(); got != tc.blocked {
				t.Fatalf("blocked=%v want=%v", got, tc.blocked)
			}
		})
	}
}
