package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (h *harness) config() appConfig {
	return appConfig{Dir: h.dir, HomeDir: h.app.eng.cfg.Resources.home, Model: "fixture", Client: h.client, Store: h.store}
}

// Launching dashi and quitting at once, or running -p, must not hide the
// session that holds the work. An empty session leaves nothing on disk.
func TestResumeSkipsEmptySessions(t *testing.T) {
	h := newHarness(t, say("Answer one."), say("Print answer."), say("Follow up."))
	h.input("real work")
	work := h.app.eng.Session().ID
	h.app.eng.Close()

	// Launch and quit with no turn.
	h.ui = newRecorderUI()
	h.build(Options{})
	empty := h.app.eng.Session().ID
	h.app.eng.Close()
	if _, err := os.Stat(h.store.Root() + "/" + empty); !os.IsNotExist(err) {
		t.Fatalf("the empty session was kept: %v", err)
	}

	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	if got := h.app.eng.Session().ID; got != work {
		t.Fatalf("-c opened %s, want %s", got, work)
	}
	h.app.eng.Close()

	// A print run with -c adds to the same session.
	var out, errOut bytes.Buffer
	code := runPrint(t.Context(), h.config(),
		Options{Resume: true, Print: "again", PrintSet: true}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d %s", code, errOut.String())
	}
	latest, err := h.store.Latest(h.dir)
	if err != nil || latest.ID != work || turnsOf(t, h.store, latest) != 2 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}

	// A print run without -c makes its own saved session. The rule: it is
	// saved, and -c picks the newest session that has a turn.
	code = runPrint(t.Context(), h.config(),
		Options{Print: "solo", PrintSet: true}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d %s", code, errOut.String())
	}
	list, _ := h.store.List(h.dir)
	if len(list) != 2 {
		t.Fatalf("sessions=%d, want 2", len(list))
	}
}

// Enter after a model-call limit stop continues the same turn, with no
// error, in the live session and after a reopen.
func TestEnterAfterLimitStopContinues(t *testing.T) {
	script := []reply{
		callTool("c1", "list_directory", `{}`),
		callTool("c2", "list_directory", `{}`),
		say("Half done."),
		callTool("c3", "list_directory", `{}`),
		say("All done."),
	}
	h := newHarnessLimit(t, 3, script...)
	h.input("do a lot")
	h.input("")
	mustHave(t, h.ui.text(), "All done.")
	if strings.Contains(h.ui.text(), "warning") {
		t.Fatalf("a warning was shown:\n%s", h.ui.text())
	}
	if n := h.turns(); n != 1 {
		t.Fatalf("turns=%d", n)
	}

	// After a reopen.
	h = newHarnessLimit(t, 3, script...)
	h.input("do a lot")
	h.app.eng.Close()
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	h.input("")
	mustHave(t, h.ui.text(), "All done.")
	if strings.Contains(h.ui.text(), "warning") {
		t.Fatalf("a warning was shown:\n%s", h.ui.text())
	}
}

// Ctrl-C keeps a queued steer for the resumed turn.
func TestCtrlCKeepsQueuedSteer(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started), say("Checked b.txt too."))
	h.app.ctrl.Input("start")
	<-started
	h.app.ctrl.Input("also check b.txt")
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "1 queued steer kept for the next turn.")
	h.input("")
	mustHave(t, h.ui.text(), "steer delivered: also check b.txt")
	in := h.client.inputAt(len(h.client.inputs) - 1)
	if !strings.Contains(inputText(in), "also check b.txt") {
		t.Fatalf("the model did not get the steer:\n%s", inputText(in))
	}
}

// A slash command typed while a turn runs is a command, not a steer.
func TestSlashCommandDuringTurnIsNotSteer(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started))
	h.app.ctrl.Input("start")
	<-started
	h.app.ctrl.Input("/model gpt-6-luna")
	h.app.ctrl.Input("/etc/hosts looks wrong")
	text := h.ui.text()
	mustHave(t, text, "/model must wait until the turn ends. Submit it again then")
	if strings.Contains(text, "steer queued: /model") {
		t.Fatalf("a command became a steer:\n%s", text)
	}
	mustHave(t, text, "Unknown command /etc/hosts")
	if strings.Contains(text, "steer queued: /etc/hosts") {
		t.Fatal("unknown slash command became a steer")
	}
	if h.app.eng.Model() != "fixture" {
		t.Fatalf("model changed to %s", h.app.eng.Model())
	}
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
}

// The footer of a turn names the model that ran it, not the current one.
func TestFooterShowsModelOfTheTurn(t *testing.T) {
	h := newHarness(t, say("One."), say("Two."))
	h.input("first")
	h.input("/model gpt-6-luna")
	h.input("second")
	mustHave(t, h.ui.text(), "turn 2 · gpt-6-luna ·")
	h.app.eng.Close()
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	text := h.ui.text()
	mustHave(t, text, "turn 1 · fixture ·")
	mustHave(t, text, "turn 2 · gpt-6-luna ·")
}

// Terminal control characters in model-controlled text never reach the UI.
func TestControlCharactersAreNeverShown(t *testing.T) {
	h := newHarness(t,
		callTool("c1", "bash", `{"command":"echo \u001b[2J\rEVIL"}`),
		say("\u001b[31mred\rtext"),
	)
	h.input("run it")
	if len(h.ui.approvals) != 1 {
		t.Fatalf("approvals=%d", len(h.ui.approvals))
	}
	req := h.ui.approvals[0]
	for _, s := range []string{req.Summary, req.Command, req.Preview, req.Dir} {
		if strings.ContainsAny(s, "\x1b\r") {
			t.Fatalf("raw control character in approval field %q", s)
		}
	}
	if !strings.Contains(req.Command, "␛") || !strings.Contains(req.Command, "␍") {
		t.Fatalf("command=%q", req.Command)
	}
	for _, b := range h.ui.blocks() {
		for _, s := range append([]string{b.Text, b.Name, b.Arg}, b.Lines...) {
			if strings.ContainsAny(s, "\x1b\r") {
				t.Fatalf("raw control character in block %+v", b)
			}
		}
	}
	mustHave(t, h.ui.text(), "␛[31mred␍text")
}

func TestShellProgressLinesShowLive(t *testing.T) {
	r := NewRenderer(1, "m", "/repo")
	data, _ := json.Marshal(map[string]any{"lines": []string{"a", "b", "c", "d"}})
	b := onlyBlock(t, r.Event(dashi.Event{Progress: &dashi.ToolProgress{
		CallID:   "c1",
		Name:     "bash",
		Progress: tool.Progress{Message: "d", Data: data},
	}}))
	if strings.Join(b.Lines, "|") != "b|c|d" {
		t.Fatalf("lines=%v", b.Lines)
	}
}

func TestToolPathsShowRelativeAndKeepTheFileName(t *testing.T) {
	inside := mainArg(json.RawMessage(`{"path":"/repo/pkg/a.go"}`), "/repo")
	if inside != "pkg/a.go" {
		t.Fatalf("inside=%q", inside)
	}
	outside := mainArg(json.RawMessage(`{"path":"/etc/hosts"}`), "/repo")
	if outside != "/etc/hosts" {
		t.Fatalf("outside=%q", outside)
	}
	long := "very/long/directory/name/that/goes/on/and/on/and/on/file.go"
	got := fitArg(long, 30)
	if !strings.HasSuffix(got, "and/on/file.go") || !strings.HasPrefix(got, "...") || len([]rune(got)) != 30 {
		t.Fatalf("got=%q", got)
	}
	cmd := fitArg("go test ./... -run TestSomethingVeryLong -v", 20)
	if !strings.HasPrefix(cmd, "go test") || !strings.HasSuffix(cmd, "...") {
		t.Fatalf("cmd=%q", cmd)
	}
}

// A new prompt after a limit stop cancels the limited turn and starts another.
func TestNewPromptAfterLimitStopCancelsIt(t *testing.T) {
	h := newHarnessLimit(t, 2,
		callTool("c1", "list_directory", `{}`),
		say("Half."),
		say("Fresh."),
	)
	h.input("big task")
	h.input("something else")
	mustHave(t, h.ui.text(), "Turn 1 canceled.")
	mustHave(t, h.ui.text(), "Fresh.")
	if strings.Contains(h.ui.text(), "warning") {
		t.Fatalf("warning:\n%s", h.ui.text())
	}
}

// A steer that misses the last permitted model call stays for the same
// turn. Enter continues that turn and delivers it.
func TestMissedSteerKeepsLimitStoppedTurn(t *testing.T) {
	var h *harness
	steerLate := func(ctx context.Context, call llm.Call) (llm.Result, error) {
		h.app.ctrl.Input("also check b.txt")
		return say("Half done.")(ctx, call)
	}
	h = newHarnessLimit(t, 3,
		callTool("c1", "list_directory", `{}`),
		callTool("c2", "list_directory", `{}`),
		steerLate,
		say("All done."),
	)
	h.input("do a lot")
	if !h.app.eng.Last(t.Context()).LimitStopped() {
		t.Fatalf("the turn is not limit-stopped:\n%s", h.ui.text())
	}
	if n := h.turns(); n != 1 {
		t.Fatalf("turns=%d, the steer became a new turn", n)
	}
	h.input("")
	mustHave(t, h.ui.text(), "steer delivered: also check b.txt")
	mustHave(t, h.ui.text(), "All done.")
	if n := h.turns(); n != 1 {
		t.Fatalf("turns=%d", n)
	}
}

// Selecting the open session from /sessions reopens it. The process holds
// its lock already.
func TestSelectingTheOpenSessionReopensIt(t *testing.T) {
	h := newHarness(t, say("First answer."))
	h.input("hello")
	id := h.app.eng.Session().ID
	h.ui.resets = 0
	h.app.ctrl.openSession(id)
	if strings.Contains(h.ui.text(), "in use") ||
		strings.Contains(h.ui.text(), "warning") {
		t.Fatalf("reopening failed:\n%s", h.ui.text())
	}
	if h.app.eng.Session().ID != id || h.ui.resets != 1 {
		t.Fatalf("session=%s resets=%d", h.app.eng.Session().ID, h.ui.resets)
	}
	mustHave(t, h.ui.text(), "First answer.")
}

// Line mode shows a finished reply that differs from the streamed text.
func TestLineModeShowsCorrectedAnswer(t *testing.T) {
	var out bytes.Buffer
	u := newLineUI(&out)
	u.Emit(Block{ID: "a", Kind: KindAssistant, Text: "The sky is red."})
	u.Emit(Block{ID: "a", Kind: KindAssistant, Text: "The sky is blue.", Done: true})
	got := out.String()
	if !strings.Contains(got, "[Corrected reply]\nThe sky is blue.") {
		t.Fatalf("output:\n%s", got)
	}
	// An append is still printed as a stream.
	out.Reset()
	u = newLineUI(&out)
	u.Emit(Block{ID: "b", Kind: KindAssistant, Text: "Hel"})
	u.Emit(Block{ID: "b", Kind: KindAssistant, Text: "Hello", Done: true})
	if out.String() != "Hello" {
		t.Fatalf("output %q", out.String())
	}
}
