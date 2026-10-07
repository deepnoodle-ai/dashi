package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// recoveryCLIClient uses the real CLI read tool and saved-session path.
// Extra prepared bytes model a protected provider replay dependency.
type recoveryCLIClient struct {
	*activeCLIClient
	outputLimits      int
	protectedOverflow bool
	answerNext        bool
}

func (c *recoveryCLIClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.activeCLIClient.Prepare(ctx, in)
	if err != nil {
		return req, err
	}
	c.mu.Lock()
	overflow := c.protectedOverflow && c.ordinary > 0 && len(in.Tools) > 0
	c.mu.Unlock()
	if !overflow {
		return req, nil
	}
	snap := req.Snapshot()
	snap.Body = append(snap.Body, []byte(strings.Repeat(" ", 70000))...)
	return llm.NewRequest(snap)
}

func (c *recoveryCLIClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	c.mu.Lock()
	answer := c.answerNext
	summary := len(call.Request.Snapshot().Input.Tools) == 0
	if answer {
		c.sends++
		if summary {
			c.summaries++
		} else {
			c.ordinary++
		}
	}
	c.mu.Unlock()
	if answer {
		text := "The saved finding is: " + activeDiagnostic
		if summary {
			text = "<summary>" + text + "</summary>"
		}
		r, err := say(text)(ctx, call)
		r.Requested = llm.ModelRef{Provider: "test", Model: call.Request.Snapshot().Input.Model}
		return r, err
	}
	r, err := c.activeCLIClient.Send(ctx, call)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && len(call.Request.Snapshot().Input.Tools) == 0 && c.outputLimits > 0 {
		c.outputLimits--
		r.Stop.Kind = llm.StopOutputLimit
	}
	return r, err
}

func TestAutomationNewPromptRecoversContextStops(t *testing.T) {
	for _, reason := range []string{"context window", "failed retry"} {
		t.Run(reason, func(t *testing.T) {
			cfg, base := activeFixtureConfig(t, false)
			client := &recoveryCLIClient{activeCLIClient: base}
			if reason == "context window" {
				client.protectedOverflow = true
				if err := os.WriteFile(filepath.Join(cfg.Dir, "diagnostic.txt"), []byte(activeDiagnostic+"\n"+strings.Repeat("source evidence retained in the full record\n", 1600)), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				client.outputLimits = 2
			}
			cfg.Client = client
			a, err := buildApp(t.Context(), cfg, newRecorderUI())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			result, err := a.eng.Prompt(t.Context(), activeObjective)
			if err != nil {
				t.Fatal(err)
			}
			want := string(dashi.LimitContextWindow)
			if reason == "failed retry" {
				want = dashi.StopReasonCompactionFailed
			}
			if result.Outcome.Stop.Reason != want {
				t.Fatalf("stop=%+v", result.Outcome.Stop)
			}
			id := a.eng.Session().ID
			a.eng.Close()
			client.mu.Lock()
			priorOrdinary := client.ordinary
			client.toolActions = client.ordinary
			client.protectedOverflow = false
			client.answerNext = true
			client.mu.Unlock()
			opts := Options{Run: true, Resume: true, ResumeID: id, Prompt: "What was the finding?", Yes: true}
			if reason == "failed retry" {
				opts.CompactionThreshold = 8000
				cfg.CompactionThreshold = opts.CompactionThreshold
			}
			code, out, diag := runAutomationFixture(t, cfg, opts, "")
			if code != 0 || !strings.Contains(out, activeDiagnostic) || strings.Contains(diag, "blocks a new request") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
			}
			if got := strings.Count(diag, "Compaction completed."); got != 1 {
				t.Fatalf("history completion notices=%d stderr=%q", got, diag)
			}
			client.mu.Lock()
			ordinary := client.ordinary
			client.mu.Unlock()
			if ordinary != priorOrdinary+1 {
				t.Fatalf("prior tool work repeated: ordinary calls=%d -> %d", priorOrdinary, ordinary)
			}
			t.Logf("%s: saved parent safely closed; one history compaction; next prompt answered; previous tool work retained", reason)
			t.Logf("CLI transcript (keyless fixture):\n%s%s", diag, out)
		})
	}
}

func TestAutomationContextRecoveryKeepsPendingGates(t *testing.T) {
	view := dashi.TurnView{ID: "parent", LastSeq: 1, Stop: dashi.Stop{Kind: dashi.StopFailed, Reason: dashi.StopReasonCompactionFailed}}
	for _, tc := range []struct {
		name    string
		pending bool
		child   dashi.Next
		missing bool
		kind    dashi.StopKind
		reason  string
		blocked bool
	}{
		{"settled failure", false, "", false, dashi.StopFailed, dashi.StopReasonCompactionFailed, false},
		{"settled limit", false, "", false, dashi.StopLimited, string(dashi.LimitContextWindow), false},
		{"pending effect", true, "", false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"pending child", false, dashi.NextResolvePending, false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"unknown child", false, dashi.NextRecover, false, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"missing notice", false, "", true, dashi.StopFailed, dashi.StopReasonCompactionFailed, true},
		{"unrelated failure", false, "", false, dashi.StopFailed, "provider failed", true},
		{"running parent", false, "", false, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := view
			parent.Stop = dashi.Stop{Kind: tc.kind, Reason: tc.reason}
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
