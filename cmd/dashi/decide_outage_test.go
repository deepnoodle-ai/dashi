package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

func TestDecideLateCommandAnswerKeepsTheConfirmedPause(t *testing.T) {
	rt := newDecideRuntime(
		decideSettings{On: []string{decideCommandRisk}},
		decideSetup{},
	)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rt.now = func() time.Time { return now }
	pause := &toolcheck.PausedError{
		Until: now.Add(time.Minute),
		Cause: decide.ErrServer,
	}
	rt.record(decideCommandRisk, &rt.commands, false,
		toolcheck.Verdict{Requests: 2}, pause)
	// This answer belongs to a check that was already in flight.
	rt.record(decideCommandRisk, &rt.commands, false,
		toolcheck.Verdict{Checked: true, Requests: 1}, nil)
	until, _, ok := rt.pausedUntil(decideCommandRisk)
	if !ok || !until.Equal(pause.Until) {
		t.Fatal("a late answer erased or changed the confirmed pause")
	}
	mustHave(t, rt.status(), "command_risk paused (server error)")
	if rt.totals.Requests != 3 || rt.commands.Checked != 1 || rt.commands.Unchecked != 1 {
		t.Fatalf("counts = %+v, %+v", rt.totals, rt.commands)
	}
	now = pause.Until
	if rt.status() != "" {
		t.Fatal("the confirmed pause survived its expiry")
	}
}

// Only the fixed availability request succeeds. Its high probability must
// not become a verdict on the failed command.
func payloadFailure(req *decide.Request) (*decide.Response, error) {
	var state struct {
		Command string `json:"command"`
	}
	b, _ := json.Marshal(req.State)
	json.Unmarshal(b, &state)
	if state.Command != "true" {
		return nil, decide.ErrServer
	}
	return &decide.Response{
		Model: "jev-test",
		Answers: map[string]decide.Answer{
			"severe": decidetest.NoulAnswer(0.99),
		},
		Usage: decide.Usage{InputTokens: 5, OutputTokens: 1},
	}, nil
}

func TestDecidePayloadFailureAsksWithoutPausing(t *testing.T) {
	for _, tc := range []struct {
		name string
		yes  bool
	}{
		{name: "default"},
		{name: "bypass", yes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, srv := decideHarness(t, Options{Yes: tc.yes},
				callTool("c1", "bash", `{"command":"echo payload"}`), say("One."),
				callTool("c2", "bash", `{"command":"echo payload"}`), say("Two."),
			)
			srv.Respond(payloadFailure)
			h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
				a := h.ui.lastApproval()
				<-a.Check.done
				if tc.yes && a.offersRemember() {
					t.Error("an unchecked bypass prompt offered remembered approval")
				}
				return Approval{
					Allow:    tc.yes,
					Remember: tc.yes,
					Reason:   declinedReason,
				}, nil
			}
			h.input("first")
			h.input("second")
			if len(h.ui.approvals) != 2 || len(srv.Requests()) != 4 {
				t.Fatalf("prompts = %d, calls = %d", len(h.ui.approvals), len(srv.Requests()))
			}
			rt := h.app.ctrl.decide
			if rt.status() != "" {
				t.Fatalf("isolated failure paused: %s", rt.status())
			}
			if rt.totals.Requests != 4 || rt.totals.InputTokens != 10 || rt.totals.OutputTokens != 2 {
				t.Fatalf("observations = %+v", rt.totals)
			}
			if rt.commands.Checked != 0 || rt.commands.Unchecked != 2 {
				t.Fatalf("command counts = %+v", rt.commands)
			}
			v := decideVerdict(t, h, 1, "c1")
			if !strings.Contains(v.Reason, "decide: unchecked (server error)") {
				t.Fatalf("recorded decision = %+v", v)
			}
		})
	}
}

func TestDecideRunYesRefusesAnUnconfirmedOutage(t *testing.T) {
	srv := decidetest.NewServer(t)
	srv.Respond(payloadFailure)
	h := newHarness(t, callTool("c1", "bash", `{"command":"echo payload"}`), say("Refused."))
	var stdout, stderr bytes.Buffer
	cfg := appConfig{
		Dir:          h.dir,
		HomeDir:      h.home,
		Model:        "fixture",
		Client:       h.client,
		Store:        h.store,
		ApproveAll:   true,
		Decide:       decideSettings{On: []string{decideCommandRisk}, Source: "--decide"},
		decideClient: srv.Client(t, decide.WithMaxRetries(5)),
	}
	if code := runPrint(t.Context(), cfg, Options{Run: true, Prompt: "go", Yes: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	in := inputText(h.client.inputAt(1))
	mustHave(t, in, "could not check this command (server error)")
	mustHave(t, in, "; decide: unchecked (server error)")
	if len(srv.Requests()) != 2 {
		t.Fatalf("calls = %d", len(srv.Requests()))
	}
}

func TestDecidePayloadFailureAsksForDelegatedCommand(t *testing.T) {
	srv := decidetest.NewServer(t)
	srv.Respond(payloadFailure)
	client := &taskFixtureClient{childCommand: "echo child payload"}
	ui := newRecorderUI()
	ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Reason: declinedReason}, nil
	}
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ApproveAll = true
	cfg.Decide = decideSettings{On: []string{decideCommandRisk}, Source: "--decide"}
	cfg.decideClient = srv.Client(t, decide.WithMaxRetries(5))
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	// This test qualifies the child's approval, not the parent's automatic
	// notice driver. Keep its independent lifecycle out of test teardown.
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = false
	a.ctrl.mu.Unlock()
	t.Cleanup(func() { a.ctrl.CancelWork(); a.eng.closeTasks(); a.ctrl.Wait(); a.eng.Close() })
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if len(srv.Requests()) != 2 {
		t.Fatalf("calls = %d", len(srv.Requests()))
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(ui.approvals) != 1 {
		t.Fatalf("approvals = %d", len(ui.approvals))
	}
	got := ui.approvals[0]
	if got.Task == "" || !got.Hold || got.offersRemember() {
		t.Fatalf("delegated approval = %+v", got)
	}
	if got.checkLine() != "decide: unchecked (server error)" {
		t.Fatalf("check = %s", got.checkLine())
	}
	if a.ctrl.decide.status() != "" {
		t.Fatal("delegated payload failure paused all commands")
	}
}
