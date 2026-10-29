package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
	"github.com/deepnoodle-ai/dashi/decide/toolcheck"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
	"github.com/deepnoodle-ai/wonton/tui"
)

// decideHarness is a harness with the command-risk check on, answered by a
// fake decide server that does not retry.
func decideHarness(t *testing.T, o Options, script ...reply) (*harness, *decidetest.Server) {
	t.Helper()
	srv := decidetest.NewServer(t)
	h := newHarness(t, script...)
	h.decideOn = decideSettings{On: []string{decideCommandRisk}, Source: "--decide"}
	h.decideClient = srv.Client(t, decide.WithMaxRetries(0))
	h.decideWait = 2 * time.Second
	h.build(o)
	return h, srv
}

// lastApproval returns the last prompt the UI showed.
func (u *recorderUI) lastApproval() toolApproval {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.approvals[len(u.approvals)-1]
}

// decideVerdict returns the recorded BeforeTool verdict of call id in
// turn n.
func decideVerdict(t *testing.T, h *harness, n int, id string) dashi.ToolVerdict {
	t.Helper()
	v, err := h.turn(n).View()
	if err != nil {
		t.Fatal(err)
	}
	return v.Tools[id].Verdict
}

// In default mode the prompt shows with the check; a flagged command
// offers allow once and deny only, and the record keeps the answer.
func TestDecideFlagsCommandInPrompt(t *testing.T) {
	h, srv := decideHarness(t, Options{},
		callTool("c1", "bash", `{"command":"echo flagged"}`), say("Stopped."))
	srv.Answer("severe", decidetest.NoulAnswer(0.91))
	var remember bool
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		a := h.ui.lastApproval()
		<-a.Check.done
		remember = a.offersRemember()
		mustHave(t, a.checkLine(), "decide: severe 91% — flagged")
		return Approval{Reason: declinedReason}, nil
	}
	h.input("clean up")
	if remember {
		t.Fatal("a flagged prompt offered allow for this run")
	}
	v := decideVerdict(t, h, 1, "c1")
	if v.Kind != dashi.DecisionDeny || v.Reason != declinedReason ||
		!strings.HasPrefix(v.RulesFinding, "severe 91% flagged (") || !strings.HasPrefix(v.RulesVersion, "decide/") {
		t.Fatalf("verdict = %+v", v)
	}
	text := h.ui.text()
	mustHave(t, text, "decide command: severe 91% — flagged")
	mustHave(t, text, "decide 1 checked, 1 flagged")
	// The model reads the person's denial, never the check's score.
	in := inputText(h.client.inputAt(1))
	mustHave(t, in, declinedReason)
	if strings.Contains(in, "91%") || strings.Contains(in, "decide") {
		t.Fatalf("the model read the decide answer:\n%s", in)
	}
	if strings.Contains(text, declinedReason+";") {
		t.Fatalf("the row repeats the decide line in the denial:\n%s", text)
	}
}

// An answer given before the check cancels it, and the record says so.
func TestDecideAnsweredFirst(t *testing.T) {
	h, srv := decideHarness(t, Options{},
		callTool("c1", "bash", `{"command":"echo first"}`), say("Done."))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.Respond(func(*decide.Request) (*decide.Response, error) {
		<-release
		return nil, errors.New("late")
	})
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Allow: true}, nil
	}
	h.input("go")
	v := decideVerdict(t, h, 1, "c1")
	if v.RulesFinding != "not checked (answered first)" || v.RulesVersion != "decide/unchecked" || strings.Contains(v.Reason, "decide") {
		t.Fatalf("verdict = %+v", v)
	}
	mustHave(t, h.ui.text(), "first")
}

// A command remembered with "a" runs again without a check.
func TestDecideRememberedCommandIsNotChecked(t *testing.T) {
	h, srv := decideHarness(t, Options{},
		callTool("c1", "bash", `{"command":"echo again"}`), say("One."),
		callTool("c2", "bash", `{"command":"echo again"}`), say("Two."))
	srv.Answer("severe", decidetest.NoulAnswer(0.02))
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		<-h.ui.lastApproval().Check.done
		return Approval{Allow: true, Remember: true}, nil
	}
	h.input("first")
	h.input("second")
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("decide requests = %d; want 1", n)
	}
	if len(h.ui.approvals) != 1 {
		t.Fatalf("approvals = %d; want 1", len(h.ui.approvals))
	}
	mustHave(t, h.ui.text(), "decide ✓")
}

// Bypass mode runs a command decide does not flag, and asks about one it
// flags, with allow once and deny only.
func TestDecideBypassHoldsFlaggedCommand(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true},
		callTool("c1", "bash", `{"command":"echo routine"}`),
		callTool("c2", "bash", `{"command":"echo severe"}`),
		say("Done."))
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		p := 0.03
		if strings.Contains(string(mustJSON(t, req)), "echo severe") {
			p = 0.89
		}
		return &decide.Response{Model: "jev-test", RequestID: "r_1",
			Answers: map[string]decide.Answer{"severe": &decide.NoulAnswer{Noul: p}}}, nil
	})
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Reason: declinedReason}, nil
	}
	h.input("work")
	if len(h.ui.approvals) != 1 {
		t.Fatalf("approvals = %d; want 1", len(h.ui.approvals))
	}
	a := h.ui.approvals[0]
	if !a.Hold || a.offersRemember() || a.Command != "echo severe" {
		t.Fatalf("approval = %+v", a)
	}
	if v := decideVerdict(t, h, 1, "c1"); v.Kind != dashi.DecisionAllow || !strings.HasPrefix(v.RulesFinding, "severe 3% (jev-test, request ") {
		t.Fatalf("routine verdict = %+v", v)
	}
	if v := decideVerdict(t, h, 1, "c2"); v.Kind != dashi.DecisionDeny {
		t.Fatalf("severe verdict = %+v", v)
	}
	text := h.ui.text()
	mustHave(t, text, "decide ✓")
	mustHave(t, text, "decide 2 checked, 1 flagged")
	// While it waits, the held call's row says so, without a spinner or
	// the flagged line, which the prompt shows.
	var states []string
	h.ui.mu.Lock()
	for _, b := range h.ui.log {
		if b.ID == "t:c2" && !b.Done && b.Decide == nil {
			states = append(states, b.State)
			if len(b.Lines) != 0 {
				t.Errorf("held row lines = %q", b.Lines)
			}
		}
	}
	h.ui.mu.Unlock()
	if !slices.Contains(states, StateChecking) || !slices.Contains(states, StateWaitingForYou) {
		t.Fatalf("held row states = %q", states)
	}
}

// mustJSON encodes v for a test to search.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// In bypass mode a timeout asks, as a flag does; it never pauses.
func TestDecideBypassAsksOnTimeout(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true},
		callTool("c1", "bash", `{"command":"echo slow"}`), say("Done."))
	h.decideWait = 50 * time.Millisecond
	h.build(Options{Yes: true})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.Respond(func(*decide.Request) (*decide.Response, error) {
		<-release
		return nil, errors.New("late")
	})
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Allow: true}, nil
	}
	h.input("go")
	if len(h.ui.approvals) != 1 || h.ui.approvals[0].checkLine() != "decide: unchecked (timeout)" {
		t.Fatalf("approvals = %+v", h.ui.approvals)
	}
	if s := h.app.ctrl.decide.status(); s != "" {
		t.Fatalf("status = %q; a timeout must not pause", s)
	}
}

// In bypass mode a server error asks, as a flag does; it never pauses, so
// one command cannot turn the check off for the others.
func TestDecideBypassAsksOnServerError(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true},
		callTool("c1", "bash", `{"command":"echo broken"}`), say("Done."))
	srv.FailNext(http.StatusInternalServerError)
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Allow: true}, nil
	}
	h.input("go")
	if len(h.ui.approvals) != 1 || h.ui.approvals[0].checkLine() != "decide: unchecked (server error)" {
		t.Fatalf("approvals = %+v", h.ui.approvals)
	}
	if s := h.app.ctrl.decide.status(); s != "" {
		t.Fatalf("status = %q; a server error must not pause", s)
	}
}

// When decide is not set up, such as after a failed login, the check
// pauses: bypass mode runs the command, and the status area shows the
// pause.
func TestDecideBypassRunsWhilePaused(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true},
		callTool("c1", "bash", `{"command":"echo paused"}`), say("Done."))
	srv.FailNext(http.StatusUnauthorized)
	h.input("go")
	if len(h.ui.approvals) != 0 {
		t.Fatalf("approvals = %d; want 0", len(h.ui.approvals))
	}
	mustHave(t, h.ui.text(), "decide command: unchecked (auth failed)")
	status := h.app.ctrl.decide.status()
	if !strings.HasPrefix(status, "decide: command_risk paused (auth failed), resumes in ") {
		t.Fatalf("status = %q", status)
	}
}

// A check that began before a pause and answers during it leaves the
// status showing the pause until it ends, since toolcheck still skips
// checks until then.
func TestDecidePauseOutlastsLateSuccess(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	rt := newDecideRuntime(decideSettings{On: []string{decideCommandRisk}}, decideSetup{})
	rt.now = func() time.Time { return now }
	pause := &toolcheck.PausedError{Until: now.Add(time.Minute), Cause: decide.ErrAuth}
	rt.record(decideCommandRisk, &rt.commands, false, toolcheck.Verdict{}, pause)
	rt.record(decideCommandRisk, &rt.commands, false, toolcheck.Verdict{Model: "jev"}, nil)
	if s := rt.status(); s != "decide: command_risk paused (auth failed), resumes in 60s" {
		t.Fatalf("status after a late success = %q", s)
	}
	now = now.Add(time.Minute)
	rt.record(decideCommandRisk, &rt.commands, false, toolcheck.Verdict{Model: "jev"}, nil)
	if _, _, ok := rt.pausedUntil(decideCommandRisk); ok {
		t.Fatal("a success after the pause ended must clear it")
	}
	if s := rt.status(); s != "" {
		t.Fatalf("status after the pause ended = %q", s)
	}
}

// With no one to answer, run --yes refuses a flagged command with a reason
// that names the check.
func TestDecideRunYesRefusesFlaggedCommand(t *testing.T) {
	srv := decidetest.NewServer(t)
	srv.Answer("severe", decidetest.NoulAnswer(0.95))
	h := newHarness(t, callTool("c1", "bash", `{"command":"echo nope"}`), say("Refused."))
	var stdout, stderr bytes.Buffer
	cfg := appConfig{
		Dir: h.dir, HomeDir: h.home, Model: "fixture", Client: h.client, Store: h.store,
		ApproveAll:   true,
		Decide:       decideSettings{On: []string{decideCommandRisk}, Source: "--decide"},
		decideClient: srv.Client(t, decide.WithMaxRetries(0)),
	}
	if code := runPrint(t.Context(), cfg, Options{Run: true, Prompt: "go", Yes: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d\nstderr:\n%s", code, stderr.String())
	}
	in := inputText(h.client.inputAt(1))
	mustHave(t, in, "The decide command-risk check flagged this command, and no one can approve it")
	if strings.Contains(in, "95%") {
		t.Fatalf("the model read the check's score:\n%s", in)
	}
}

// The record line is a format the renderer parses on reopen; this pins it.
func TestDecideRecordFormat(t *testing.T) {
	cases := []struct {
		r          decideResult
		line, rule string
		mark       decideMark
	}{
		{decideResult{Checked: true, Flagged: true, Severe: 0.919, Model: "jev-1.13.0", Request: "r_12"},
			"severe 91% flagged (jev-1.13.0, request r_12)", "decide/jev-1.13.0",
			decideMark{Check: "command", Flagged: true, Text: "severe 91% — flagged"}},
		{decideResult{Checked: true, Severe: 0.7999, Model: "jev-1.13.0", Request: "r_13"},
			"severe 79% (jev-1.13.0, request r_13)", "decide/jev-1.13.0",
			decideMark{Check: "command", OK: true, Text: "severe 79% — ok"}},
		{decideResult{Cause: "timeout"}, "unchecked (timeout)", "decide/unchecked",
			decideMark{Check: "command", Text: "unchecked (timeout)"}},
		{decideResult{AnsweredFirst: true}, "not checked (answered first)", "decide/unchecked",
			decideMark{Check: "command", Text: "not checked (answered first)"}},
	}
	for _, tc := range cases {
		if got := tc.r.recordLine(); got != tc.line {
			t.Errorf("recordLine = %q; want %q", got, tc.line)
		}
		if got := tc.r.rulesVersion(); got != tc.rule {
			t.Errorf("rulesVersion = %q; want %q", got, tc.rule)
		}
		if m := markFromRecord(tc.r.rulesVersion(), tc.r.recordLine()); m == nil || *m != tc.mark {
			t.Errorf("mark of %q = %+v; want %+v", tc.r.recordLine(), m, tc.mark)
		}
	}
	if markFromRecord("policy-v1", "severe 91% flagged (x)") != nil {
		t.Error("a finding without the decide rules version was read as checked")
	}
}

// A pause, and failures the command can cause, which ask, each name their
// cause.
func TestDecideCause(t *testing.T) {
	cases := []struct {
		err    error
		cause  string
		paused bool
	}{
		{context.DeadlineExceeded, "timeout", false},
		{toolcheck.ErrTooLarge, "too large", false},
		{fmt.Errorf("x: %w", decide.ErrNoAPIKey), "no credentials", false},
		{fmt.Errorf("x: %w", decide.ErrServer), "server error", false},
		{fmt.Errorf("x: %w", decide.ErrRateLimited), "rate limited", false},
		{fmt.Errorf("x: %w", decide.ErrInvalidAnswer), "invalid answer", false},
		{&toolcheck.PausedError{Until: time.Now(), Cause: fmt.Errorf("x: %w", decide.ErrAuth)}, "auth failed", true},
	}
	for _, tc := range cases {
		if got := decideCause(tc.err); got != tc.cause {
			t.Errorf("decideCause(%v) = %q; want %q", tc.err, got, tc.cause)
		}
		if got := toolcheck.IsPaused(tc.err); got != tc.paused {
			t.Errorf("IsPaused(%v) = %v; want %v", tc.err, got, tc.paused)
		}
	}
}

// An MCP tool named bash is never checked as a shell, and nothing is
// checked with the feature off.
func TestDecideChecksOnlyWorkspaceBash(t *testing.T) {
	rt := newDecideRuntime(decideSettings{On: []string{decideCommandRisk}}, decideSetup{})
	plan := dashi.ToolPlan{}
	plan.Call.Name = "bash"
	ctx := context.WithValue(t.Context(), decideSlotKey{}, &decideSlot{plan: plan})
	if rt.checkedCommand(ctx, ApprovalRequest{Tool: "bash", Command: "ls"}) == nil {
		t.Fatal("workspace bash was not checked")
	}
	if rt.checkedCommand(ctx, ApprovalRequest{Tool: "bash"}) != nil {
		t.Fatal("an MCP tool named bash was checked")
	}
	if newDecideRuntime(decideSettings{}, decideSetup{}) != nil {
		t.Fatal("a runtime was made with every feature off")
	}
	var off *decideRuntime
	if off.checkedCommand(ctx, ApprovalRequest{Tool: "bash", Command: "ls"}) != nil || off.status() != "" {
		t.Fatal("a nil runtime checked or reported")
	}
}

// The flag beats DASHI_DECIDE's source name, which beats the user file; a
// project file is ignored with a warning.
func TestResolveDecide(t *testing.T) {
	user := preferenceFile{Source: "/home/u/.dashi/settings.json", Decide: json.RawMessage(`["command_risk"]`)}
	cases := []struct {
		name             string
		flag             string
		flagSet          bool
		env              string
		envSet           bool
		user             preferenceFile
		want, wantSource string
		wantErr          string
	}{
		{name: "nothing", want: "", wantSource: ""},
		{name: "user file", user: user, want: "command_risk", wantSource: user.Source},
		{name: "flag replaces file", flag: "off", flagSet: true, user: user, want: "", wantSource: "--decide"},
		{name: "variable", flag: "command_risk,turn_watcher", flagSet: true, env: "command_risk,turn_watcher", envSet: true,
			want: "command_risk,turn_watcher", wantSource: "DASHI_DECIDE"},
		{name: "unknown", flag: "commands", flagSet: true, wantErr: `unknown decide feature "commands"; use command_risk`},
		{name: "off mixed", flag: "off,command_risk", flagSet: true, wantErr: "cannot be combined"},
		{name: "bad file", user: preferenceFile{Source: "s.json", Decide: json.RawMessage(`"command_risk"`)}, wantErr: "must be a list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDecide(io.Discard, tc.flag, tc.flagSet, tc.env, tc.envSet, tc.user)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got.On, ",") != tc.want || got.Source != tc.wantSource {
				t.Fatalf("got %+v, %v; want %q from %q", got, err, tc.want, tc.wantSource)
			}
		})
	}
	var w bytes.Buffer
	warnDecideSettings(&w, preferenceFile{Source: "/repo/.dashi/settings.json", Project: true, Decide: json.RawMessage(`[]`)})
	mustHave(t, w.String(), `/repo/.dashi/settings.json: "decide" is ignored in a project settings file`)
}

// An unknown name in the user settings file, such as a removed feature, is
// ignored with a warning instead of stopping dashi.
func TestResolveDecideIgnoresUnknownSettingsName(t *testing.T) {
	user := preferenceFile{Source: "/home/u/.dashi/settings.json",
		Decide: json.RawMessage(`["selective_compaction", "command_risk"]`)}
	var w bytes.Buffer
	got, err := resolveDecide(&w, "", false, "", false, user)
	if err != nil || strings.Join(got.On, ",") != "command_risk" {
		t.Fatalf("got %+v, %v; want command_risk", got, err)
	}
	mustHave(t, w.String(), `unknown decide feature "selective_compaction" is ignored`)
}

// --decide with an unknown name is a command-line error that lists the
// valid names.
func TestDecideFlagValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	started := false
	app := newApp(&stdout, &stderr, func(Options) error { started = true; return nil })
	err := app.ExecuteContext(t.Context(), []string{"--decide", "nope"})
	if err == nil || started || !strings.Contains(err.Error(), "use command_risk, prompt_injection, turn_watcher, or off") {
		t.Fatalf("err = %v, started = %v", err, started)
	}
	var got Options
	app = newApp(&stdout, &stderr, func(o Options) error { got = o; return nil })
	if err := app.ExecuteContext(t.Context(), []string{"--decide", "command_risk"}); err != nil || got.Decide != "command_risk" || !got.DecideSet {
		t.Fatalf("err = %v, options = %+v", err, got)
	}
}

// /settings shows every feature, the command-risk scope, the provider, and
// this run's counts; --json has the same fields under decide.
func TestDecideSettingsSection(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true},
		callTool("c1", "bash", `{"command":"echo hi"}`), say("Done."))
	srv.Respond(func(*decide.Request) (*decide.Response, error) {
		return &decide.Response{Model: "jev-1.13.0", RequestID: "r_1", Usage: decide.Usage{InputTokens: 120, OutputTokens: 2},
			Answers: map[string]decide.Answer{"severe": &decide.NoulAnswer{Noul: 0.01}}}, nil
	})
	h.input("go")
	view := h.app.ctrl.inspectDecide()
	var text strings.Builder
	writeDecideInspection(&text, *view)
	for _, want := range []string{
		"command_risk: on (--decide); shell commands only",
		"turn_watcher: off (--decide)",
		"\n  bypass mode: write_file, edit_file, and MCP calls run unasked and unchecked" +
			"\n    while a check is paused, shell commands run unchecked too\n",
		"provider: ready: typesafe jev-1.13.0",
		"this run: 1 request, 120 input and 2 output tokens\n    command_risk: 1 checked, 0 flagged, 0 unchecked",
		"prompt_injection: off (--decide)\n",
	} {
		mustHave(t, text.String(), want)
	}
	// No line wraps at 80 columns, where a wrapped line loses its indent.
	for line := range strings.SplitSeq(text.String(), "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line of %d columns: %q", n, line)
		}
	}
	raw := string(mustJSON(t, settingsInspection{Decide: view}))
	for _, want := range []string{`"decide":{`, `"command_risk":{"on":true`, `"provider":"ready: typesafe jev-1.13.0"`, `"input_tokens":120`, `"counts":{"command_risk":{"checked":1,"flagged":0,"unchecked":0}}`} {
		mustHave(t, raw, want)
	}
}

// With every feature off, nothing about decide shows.
func TestDecideOffChangesNothing(t *testing.T) {
	h := newHarness(t, callTool("c1", "bash", `{"command":"echo off"}`), say("Done."))
	h.input("go")
	if h.app.ctrl.decide != nil || strings.Contains(h.ui.text(), "decide") {
		t.Fatalf("decide showed with every feature off:\n%s", h.ui.text())
	}
	if v := decideVerdict(t, h, 1, "c1"); strings.Contains(v.Reason, "decide") || v.RulesVersion != "" {
		t.Fatalf("verdict = %+v", v)
	}
}

// Line mode offers y/n only for a flagged or held command.
func TestDecideLineModeQuestion(t *testing.T) {
	if q := approvalQuestion(toolApproval{Hold: true}); q != "Allow once / deny? [y/n or n REASON] " {
		t.Fatalf("held question = %q", q)
	}
	flagged := doneCheck(decideResult{Checked: true, Flagged: true, Severe: 0.9})
	if q := approvalQuestion(toolApproval{Check: flagged}); !strings.HasPrefix(q, "Allow once / deny?") {
		t.Fatalf("flagged question = %q", q)
	}
	if q := approvalQuestion(toolApproval{}); !strings.Contains(q, "allow for this Dashi run") {
		t.Fatalf("plain question = %q", q)
	}
}

// A setup error turns every check off before the first turn: no prompt,
// row, or footer shows decide, and the status area says why. Bypass mode
// runs commands as it would without decide.
func TestDecideSetupErrorTurnsChecksOff(t *testing.T) {
	h := newHarness(t,
		callTool("c1", "bash", `{"command":"echo one"}`),
		callTool("c2", "bash", `{"command":"echo two"}`),
		say("Done."))
	h.decideOn = decideSettings{On: []string{decideCommandRisk}, Source: "DASHI_DECIDE"}
	h.decideSetup = decideSetup{
		Setup: defaultclient.Setup{Provider: "cloudfare"},
		Err:   fmt.Errorf("decide: unknown DECIDE_PROVIDER: %w", defaultclient.ErrUnknownProvider),
	}
	h.build(Options{Yes: true})
	h.input("go")
	if text := h.ui.text(); strings.Contains(text, "decide") || len(h.ui.approvals) != 0 {
		t.Fatalf("approvals = %d; decide showed with its checks off:\n%s", len(h.ui.approvals), text)
	}
	rt := h.app.ctrl.decide
	if s := rt.status(); s != `decide: off (unknown DECIDE_PROVIDER "cloudfare")` {
		t.Fatalf("status = %q", s)
	}
	if got := rt.providerState(); got != `off: unknown DECIDE_PROVIDER "cloudfare"` {
		t.Fatalf("provider = %q", got)
	}
	if v := decideVerdict(t, h, 1, "c1"); v.Kind != dashi.DecisionAllow || strings.Contains(v.Reason, "decide") {
		t.Fatalf("verdict = %+v", v)
	}
}

// A setup error turns the checks off with one warning that names the
// variables to set; an unattended run with approveAll and command_risk on
// refuses to start instead.
func TestLoadDecideSetup(t *testing.T) {
	noKey := func() (defaultclient.Setup, error) {
		return defaultclient.Setup{Provider: "typesafe"}, fmt.Errorf("decide: no credentials: set TYPESAFE_API_KEY: %w %w",
			defaultclient.ErrSetup, decide.ErrNoAPIKey)
	}
	on := decideSettings{On: []string{decideCommandRisk}, Source: "--decide"}
	for _, c := range []struct{ unattended, approveAll bool }{{false, false}, {false, true}, {true, false}} {
		setup, err := loadDecideSetup(on, c.unattended, c.approveAll, noKey)
		if err != nil || offReason(setup) != "no credentials" {
			t.Fatalf("%+v: setup = %+v, %v", c, setup, err)
		}
		if got := setup.warning(); !strings.HasPrefix(got, "decide checks are off for this run: no credentials: set TYPESAFE_API_KEY") {
			t.Fatalf("%+v: warning = %q", c, got)
		}
	}
	if _, err := loadDecideSetup(on, true, true, noKey); err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
		t.Fatalf("run --yes: err = %v", err)
	}
	loaded := false
	if _, err := loadDecideSetup(decideSettings{}, true, true, func() (defaultclient.Setup, error) {
		loaded = true
		return defaultclient.Setup{}, nil
	}); err != nil || loaded {
		t.Fatal("the decide client was loaded with every feature off")
	}
}

// bashSlot is the context a BeforeTool call of bash gives the approval
// step: the call's plan in a decideSlot, and the call that owns a prompt.
func bashSlot(ctx context.Context, id, command string) (context.Context, *decideSlot) {
	args, _ := json.Marshal(map[string]string{"command": command})
	plan := dashi.ToolPlan{Call: tool.Call{ID: id, Name: "bash", Args: args}}
	slot := &decideSlot{plan: plan}
	ctx = context.WithValue(ctx, decideSlotKey{}, slot)
	return context.WithValue(ctx, brokerOwnerKey{}, brokerOwner{Call: dashi.CallRef{TurnID: "t", CallID: id}}), slot
}

// In bypass mode the check runs before the approval queue: two checks are
// in flight at once, and a write_file call runs while they wait.
func TestDecideBypassChecksOutsideTheQueue(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true})
	var mu sync.Mutex
	arrived := 0
	bothIn, release := make(chan struct{}), make(chan struct{})
	srv.Respond(func(*decide.Request) (*decide.Response, error) {
		mu.Lock()
		if arrived++; arrived == 2 {
			close(bothIn)
		}
		mu.Unlock()
		<-release
		return &decide.Response{Model: "jev-test", RequestID: "r",
			Answers: map[string]decide.Answer{"severe": &decide.NoulAnswer{Noul: 0.02}}}, nil
	})
	c := h.app.ctrl
	results := make(chan Approval, 2)
	for _, id := range []string{"c1", "c2"} {
		go func() {
			ctx, _ := bashSlot(t.Context(), id, "echo "+id)
			a, err := c.Approve(ctx, ApprovalRequest{Tool: "bash", Command: "echo " + id})
			if err != nil {
				t.Error(err)
			}
			results <- a
		}()
	}
	select {
	case <-bothIn:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("the second check waited behind the first")
	}
	write, err := c.Approve(t.Context(), ApprovalRequest{Tool: "write_file"})
	close(release)
	if err != nil || !write.Allow {
		t.Fatalf("write_file = %+v, %v", write, err)
	}
	for range 2 {
		if a := <-results; !a.Allow {
			t.Fatalf("approval = %+v; want allowed after a quick check", a)
		}
	}
	if len(h.ui.approvals) != 0 {
		t.Fatalf("approvals = %d; want 0", len(h.ui.approvals))
	}
}

// The same holds through the real workspace policy, wired as the CLI wires
// it: the policy does not hold its lock across a check, so two bash checks
// reach decide at once, and a write_file call runs while they wait.
func TestDecideBypassChecksRunConcurrentlyThroughThePolicy(t *testing.T) {
	h, srv := decideHarness(t, Options{Yes: true})
	var mu sync.Mutex
	arrived := 0
	bothIn, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free)
	srv.Respond(func(*decide.Request) (*decide.Response, error) {
		mu.Lock()
		if arrived++; arrived == 2 {
			close(bothIn)
		}
		mu.Unlock()
		<-release
		return &decide.Response{Model: "jev-test", RequestID: "r",
			Answers: map[string]decide.Answer{"severe": &decide.NoulAnswer{Noul: 0.02}}}, nil
	})
	w, err := workspaceWiring(Env{Dir: t.TempDir(), Approve: h.app.ctrl.Approve})
	if err != nil {
		t.Fatal(err)
	}
	before := attributeToolHooks(w.Hooks).BeforeTool[0].Func
	entries, err := w.Tools.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plan := func(id, name string, args map[string]string) dashi.ToolPlan {
		raw, _ := json.Marshal(args)
		for _, e := range entries {
			if e.Definition.Name == name {
				return dashi.ToolPlan{Call: tool.Call{ID: id, Name: name, Args: raw}, Definition: e.Definition}
			}
		}
		t.Fatalf("no tool %s", name)
		return dashi.ToolPlan{}
	}
	decisions := make(chan dashi.ToolDecision, 2)
	for _, id := range []string{"c1", "c2"} {
		p := plan(id, "bash", map[string]string{"command": "echo " + id})
		go func() {
			d, err := before(t.Context(), p)
			if err != nil {
				t.Error(err)
			}
			decisions <- d
		}()
	}
	select {
	case <-bothIn:
	case <-time.After(time.Second):
		t.Fatal("the second check waited behind the first")
	}
	write, err := before(t.Context(), plan("c3", "write_file", map[string]string{"path": "a.txt", "content": "a"}))
	free()
	if err != nil || write.Kind != dashi.DecisionAllow {
		t.Fatalf("write_file = %+v, %v", write, err)
	}
	for range 2 {
		if d := <-decisions; d.Kind != dashi.DecisionAllow {
			t.Fatalf("bash decision = %+v; want allowed after a quick check", d)
		}
	}
}

// A delegated task's shell command gets the same check in bypass mode: a
// flagged command asks, names its task, and offers allow once and deny
// only.
func TestDecideBypassChecksDelegatedTask(t *testing.T) {
	srv := decidetest.NewServer(t)
	srv.Answer("severe", decidetest.NoulAnswer(0.93))
	client := &taskFixtureClient{childCommand: "git push --force origin main"}
	ui := newRecorderUI()
	ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Reason: declinedReason}, nil
	}
	cfg := testConfig(t)
	cfg.Client = client
	cfg.ApproveAll = true
	cfg.Decide = decideSettings{On: []string{decideCommandRisk}, Source: "--decide"}
	cfg.decideClient = srv.Client(t, decide.WithMaxRetries(0))
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.ctrl.CancelWork(); a.eng.closeTasks(); a.ctrl.Wait(); a.eng.Close() })
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("decide requests = %d; want 1", n)
	}
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(ui.approvals) != 1 {
		t.Fatalf("approvals = %d; want 1", len(ui.approvals))
	}
	got := ui.approvals[0]
	if got.Task == "" || !got.Hold || got.offersRemember() || got.Command != client.childCommand ||
		got.checkLine() != "decide: severe 93% — flagged" {
		t.Fatalf("approval = %+v, line %q", got, got.checkLine())
	}
}

// A flagged command is never remembered, even when the UI answers "allow
// for this run": it asks again next time.
func TestDecideFlaggedCommandIsNotRemembered(t *testing.T) {
	h, srv := decideHarness(t, Options{},
		callTool("c1", "bash", `{"command":"echo flagged"}`), say("One."),
		callTool("c2", "bash", `{"command":"echo flagged"}`), say("Two."))
	srv.Answer("severe", decidetest.NoulAnswer(0.91))
	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		<-h.ui.lastApproval().Check.done
		return Approval{Allow: true, Remember: true}, nil
	}
	h.input("first")
	h.input("second")
	if len(h.ui.approvals) != 2 || len(srv.Requests()) != 2 {
		t.Fatalf("approvals = %d, decide requests = %d; want 2 and 2: a flagged command was remembered",
			len(h.ui.approvals), len(srv.Requests()))
	}
}

// The full-screen prompt ignores "a" on a flagged command, and "y" still
// answers allow once.
func TestDecideScreenIgnoresRememberOnFlaggedPrompt(t *testing.T) {
	s := newScreen("fixture")
	s.send = func(tui.Event) {}
	p := &approvalPrompt{
		req:   toolApproval{Check: doneCheck(decideResult{Checked: true, Flagged: true, Severe: 0.9})},
		reply: make(chan Approval, 1),
	}
	s.HandleEvent(promptEvent{id: 1, approval: p})
	s.HandleEvent(tui.KeyEvent{Rune: 'a'})
	select {
	case a := <-p.reply:
		t.Fatalf("answer = %+v; want none for a", a)
	default:
	}
	s.HandleEvent(tui.KeyEvent{Rune: 'y'})
	if a := <-p.reply; !a.Allow || a.Remember {
		t.Fatalf("answer = %+v; want allow once", a)
	}
}

// A row shows "decide ✓ command" only beside a call that ran; a denied row shows
// the answer as a line, so the check's answer and the denial read apart.
func TestDecideRowMarkAgreesWithOutcome(t *testing.T) {
	ok := &decideMark{Check: "command", OK: true, Text: "severe 52% — ok"}
	ran := formatTool(Block{Kind: KindTool, Name: "bash", Arg: "ls", State: StateOK, Decide: ok})
	if !strings.Contains(ran, "decide ✓ command") || strings.Contains(ran, "52%") {
		t.Fatalf("ran row = %q", ran)
	}
	denied := formatTool(Block{Kind: KindTool, Name: "bash", Arg: "rm -rf build/", State: StateDenied, Decide: ok})
	if strings.Contains(denied, "✓") || !strings.Contains(denied, "denied\n    decide command: severe 52% — ok") {
		t.Fatalf("denied row = %q", denied)
	}
	screen := tui.SprintScreen(toolView(Block{Kind: KindTool, Name: "bash", Arg: "rm -rf build/", State: StateDenied, Decide: ok}),
		tui.WithWidth(80), tui.WithHeight(2)).Text()
	if strings.Contains(screen, "✓") || !strings.Contains(screen, "decide command: severe 52% — ok") {
		t.Fatalf("denied screen row = %q", screen)
	}
}

// A narrow footer leaves out the model call count, the model, and the
// tokens before the decide counts, so the flagged count never clips.
func TestDecideFooterKeepsFlaggedCount(t *testing.T) {
	f := footerFacts{Turn: 1, Model: "claude-haiku-4-5", Calls: 2, Tokens: 6495, Cost: "$0.0069",
		DecideChecked: 4, DecideFlagged: 1}
	if got := f.String(); !strings.HasSuffix(got, " · $0.0069 · decide 4 checked, 1 flagged") {
		t.Fatalf("plain = %q", got)
	}
	b := Block{Kind: KindFooter, Text: f.String(), Footer: &f, Done: true}
	for _, width := range []int{100, 80, 60, 40} {
		got := strings.TrimRight(tui.SprintScreen(footerView(b, width), tui.WithWidth(width), tui.WithHeight(1)).Text(), "\n")
		if !strings.Contains(got, "decide 4 checked, 1 flagged") {
			t.Errorf("width %d: %q", width, got)
		}
		if source := footerSource(b, width); !strings.Contains(got, source) {
			t.Errorf("width %d: source %q is not the shown text %q", width, source, got)
		}
	}
	if got := footerSource(b, 100); got != "turn 1 · claude-haiku-4-5 · 6,495 tokens · $0.0069 · decide 4 checked, 1 flagged" {
		t.Fatalf("100 columns = %q", got)
	}
	f.DecideFlagged = 0
	if got := f.decideText(); got != "decide 4 checked" {
		t.Fatalf("no flags = %q", got)
	}
}
