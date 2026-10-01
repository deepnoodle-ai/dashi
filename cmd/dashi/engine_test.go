package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

type noteArgs struct {
	Text string `json:"text" description:"Text to write."`
}

// withNoteTool adds a write_note tool that needs approval, on top of the
// coding tools, for the duration of the test.
func withNoteTool(t *testing.T, runs *atomicCounter) {
	t.Helper()
	old := adapters
	t.Cleanup(func() { adapters = old })
	adapters = append(slicesClone(old), func(Env) (wiring, error) {
		note, err := tool.Func(
			"write_note",
			"Write a note.",
			func(_ context.Context, _ tool.Call, in noteArgs) (tool.Outcome, error) {
				runs.Add(1)
				return tool.Succeeded("wrote " + in.Text), nil
			},
		)
		if err != nil {
			return wiring{}, err
		}
		return wiring{Tools: tool.List{note}}, nil
	})
}

func TestPromptStreamsTextAndFooter(t *testing.T) {
	h := newHarness(t, say("The fix is done."))
	h.input("fix the test")
	text := h.ui.text()
	mustHave(t, text, "> fix the test")
	mustHave(t, text, "The fix is done.")
	mustHave(t, text, "── turn 1 · fixture · 1 model call · 120 tokens")
	s := h.app.eng.Session()
	if h.turns() != 1 || s.Title != "fix the test" {
		t.Fatalf("session=%+v", s)
	}
	// Streamed text and the finished text are one block.
	assistants := 0
	for _, b := range h.ui.blocks() {
		if b.Kind == KindAssistant {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant blocks=%d", assistants)
	}
}

func TestToolStepsShowNameArgumentAndState(t *testing.T) {
	h := newHarness(t,
		callTool("c1", "read_file", `{"path":"a.txt"}`),
		say("It says hello."),
	)
	if err := os.WriteFile(filepath.Join(h.dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.input("read a.txt")
	var got Block
	for _, b := range h.ui.blocks() {
		if b.Kind == KindTool {
			got = b
		}
	}
	if got.Name != "read_file" || got.Arg != "a.txt" || got.State != StateOK || !got.Done {
		t.Fatalf("tool block=%+v", got)
	}
	mustHave(t, h.ui.text(), "read_file  a.txt")
}

func TestApprovalAnswers(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newHarness(t,
		callTool("c1", "write_note", `{"text":"one"}`),
		say("Noted."),
		callTool("c2", "write_note", `{"text":"two"}`),
		say("Could not write."),
	)
	h.input("write a note")
	if runs.Load() != 1 || len(h.ui.approvals) != 1 {
		t.Fatalf("runs=%d approvals=%d", runs.Load(), len(h.ui.approvals))
	}
	if h.ui.approvals[0].Tool != "write_note" {
		t.Fatalf("request=%+v", h.ui.approvals[0])
	}

	h.ui.answer = func(context.Context, ApprovalRequest) (Approval, error) {
		return Approval{Reason: declinedReason}, nil
	}
	h.input("write another")
	if runs.Load() != 1 {
		t.Fatalf("a denied call ran: runs=%d", runs.Load())
	}
	var denied bool
	for _, b := range h.ui.blocks() {
		if b.Kind == KindTool && b.State == StateDenied {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no denied tool line in:\n%s", h.ui.text())
	}
	// The model reads the reason.
	in := h.client.inputAt(len(h.client.inputs) - 1)
	if !strings.Contains(inputText(in), "declined") {
		t.Fatalf("the model did not read the reason:\n%s", inputText(in))
	}
}

// Quit during an approval prompt leaves the turn interrupted. After a
// reopen, an empty Enter continues it, and the same call is asked again.
func TestQuitDuringApprovalThenContinue(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newHarness(t,
		callTool("c1", "write_note", `{"text":"one"}`),
		say("Noted."),
	)
	asked := make(chan struct{})
	h.ui.answer = func(ctx context.Context, _ ApprovalRequest) (Approval, error) {
		close(asked)
		<-ctx.Done()
		return Approval{}, ctx.Err()
	}
	h.app.ctrl.Input("write a note")
	<-asked
	if h.app.ctrl.Mode() != ModeApproval {
		t.Fatalf("mode=%v", h.app.ctrl.Mode())
	}
	h.app.ctrl.QuitDuringRun("/quit")
	h.app.ctrl.Wait()
	select {
	case <-h.ui.quit:
	default:
		t.Fatal("the program did not quit")
	}
	if runs.Load() != 0 {
		t.Fatal("the call ran without approval")
	}
	last := h.app.eng.Last(t.Context())
	if !last.CanGoOn() || last.View.Stop.Kind != dashi.StopInterrupted {
		t.Fatalf("stop=%+v", last.View.Stop)
	}
	h.app.eng.Close()

	// A new process opens the same session.
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	mustHave(t, h.ui.text(), "Turn 1 was interrupted.")
	h.input("")
	if runs.Load() != 1 || len(h.ui.approvals) != 1 {
		t.Fatalf("runs=%d approvals=%d", runs.Load(), len(h.ui.approvals))
	}
	mustHave(t, h.ui.text(), "Noted.")
	last = h.app.eng.Last(t.Context())
	if last.CanGoOn() || last.View.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v", last.View.Stop)
	}
	if h.turns() != 1 {
		t.Fatalf("turns=%d", h.turns())
	}
}

func TestSteerReachesRunningTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := func(ctx context.Context, call llm.Call) (llm.Result, error) {
		close(started)
		<-release
		return callTool("c1", "list_directory", `{}`)(ctx, call)
	}
	h := newHarness(t, first, say("Using the new plan."))
	h.app.ctrl.Input("start")
	<-started
	if h.app.ctrl.Mode() != ModeRunning {
		t.Fatalf("mode=%v", h.app.ctrl.Mode())
	}
	h.app.ctrl.Input("use plan B")
	mustHave(t, h.ui.text(), "steer queued: use plan B")
	close(release)
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "steer delivered: use plan B")
	if strings.Contains(h.ui.text(), "steer queued") {
		t.Fatalf("the queued line was not replaced:\n%s", h.ui.text())
	}
	// The recorded turn holds the steer, and the model saw it.
	found := false
	for _, s := range h.turn(1).Steps {
		if s.Context != nil && s.Context.Steering {
			found = true
		}
	}
	if !found {
		t.Fatal("no steering step in the record")
	}
	in := h.client.inputAt(len(h.client.inputs) - 1)
	if !strings.Contains(inputText(in), "use plan B") {
		t.Fatalf("model input lacks the steer:\n%s", inputText(in))
	}
}

func TestLimitStopThenContinueSameTurn(t *testing.T) {
	h := newHarnessLimit(t, 3,
		callTool("c1", "list_directory", `{}`),
		callTool("c2", "list_directory", `{}`),
		say("Half done."),
		callTool("c3", "list_directory", `{}`),
		say("All done."),
	)
	h.input("do a lot")
	mustHave(t, h.ui.text(), "Stopped after 3 model calls. Press Enter or type /continue to go on.")
	if !h.app.eng.Last(t.Context()).LimitStopped() {
		t.Fatal("the turn is not limit-stopped")
	}
	h.input("/continue")
	mustHave(t, h.ui.text(), "All done.")
	if strings.Contains(h.ui.text(), "system note") {
		t.Fatal("/continue added a note")
	}
	if n := h.turns(); n != 1 {
		t.Fatalf("/continue made a new turn: %d turns", n)
	}
	last := h.app.eng.Last(t.Context())
	if last.View.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v", last.View.Stop)
	}
}

func TestContinueWithoutLimitStopExplains(t *testing.T) {
	h := newHarness(t, say("Hi."))
	h.input("hello")
	h.input("/continue")
	mustHave(t, h.ui.text(), "/continue works after a model-call limit stop.")
}

func TestNewPromptCancelsUnfinishedTurn(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started), say("Fresh answer."))
	h.app.ctrl.Input("first")
	<-started
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "Interrupted.")
	if h.app.ctrl.Interrupt() != true {
		t.Fatal("Ctrl-C at idle must exit")
	}
	h.input("second")
	mustHave(t, h.ui.text(), "Turn 1 canceled.")
	mustHave(t, h.ui.text(), "Fresh answer.")
	if n := h.turns(); n != 2 {
		t.Fatalf("turns=%d", n)
	}
}

// /model changes only the next new turn. A resumed turn keeps the model it
// recorded, even after a reopen, and the next prompt uses the new model.
func TestModelChangeThenResumeKeepsRecordedModel(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started), say("Resumed."), say("Next."))
	h.app.ctrl.Input("first")
	<-started
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "Interrupted.")
	h.input("/model gpt-6-luna")
	if h.app.eng.Model() != "gpt-6-luna" {
		t.Fatalf("model=%s", h.app.eng.Model())
	}
	h.app.eng.Close()

	// A new process resumes the session and runs the interrupted turn on.
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	if h.app.eng.Model() != "gpt-6-luna" {
		t.Fatalf("reopened model=%s", h.app.eng.Model())
	}
	h.input("")
	mustHave(t, h.ui.text(), "Resumed.")
	if got := h.client.inputAt(1).Model; got != "fixture" {
		t.Fatalf("resumed turn model=%s, want the recorded fixture", got)
	}
	h.input("second")
	mustHave(t, h.ui.text(), "Next.")
	if got := h.client.inputAt(2).Model; got != "gpt-6-luna" {
		t.Fatalf("next turn model=%s, want gpt-6-luna", got)
	}
	if n := h.turns(); n != 2 {
		t.Fatalf("turns=%d", n)
	}
}

// A reopened session shows every turn, and a follow-up sends prior history.
func TestResumeShowsScrollbackAndKeepsHistory(t *testing.T) {
	h := newHarness(t, say("First answer."), say("Second answer."))
	h.input("first question")
	h.app.eng.Close()

	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	text := h.ui.text()
	mustHave(t, text, "> first question")
	mustHave(t, text, "First answer.")
	mustHave(t, text, "── turn 1 · fixture · 1 model call · 120 tokens")

	h.input("follow up")
	in := h.client.inputAt(len(h.client.inputs) - 1)
	got := inputText(in)
	for _, want := range []string{"first question", "First answer.", "follow up"} {
		if !strings.Contains(got, want) {
			t.Fatalf("model input lacks %q:\n%s", want, got)
		}
	}
	if h.turns() != 2 {
		t.Fatalf("turns=%d", h.turns())
	}
}

// A turn record that cannot be read is skipped with a warning.
func TestResumeSkipsUnreadableTurn(t *testing.T) {
	h := newHarness(t, say("First answer."), say("Second answer."), say("Third."))
	h.input("first question")
	h.input("second question")
	rec, err := h.store.Recorder(h.app.eng.Session())
	if err != nil {
		t.Fatal(err)
	}
	bad := session.TurnID(h.app.eng.Session().ID, 1)
	if err := os.WriteFile(rec.Path(bad), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.app.eng.Close()

	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	text := h.ui.text()
	mustHave(t, text, "warning: Turn 1 skipped")
	mustHave(t, text, "Second answer.")
	h.input("third question")
	mustHave(t, h.ui.text(), "Third.")
	mustHave(t, h.ui.text(), "warning: Turn 1 skipped")
}

func TestSecondProcessOnSameSessionIsRefused(t *testing.T) {
	h := newHarness(t, say("Hi."))
	s := h.app.eng.Session()
	other := NewEngine(EngineConfig{Store: h.store, Dir: h.dir})
	if err := other.Open(s); err == nil || !strings.Contains(err.Error(), sessionstore.ErrBusy.Error()) {
		t.Fatalf("err=%v", err)
	}
}

func TestSlashCommands(t *testing.T) {
	h := newHarness(t, say("Hi."))
	h.input("/model nope")
	mustHave(t, h.ui.text(), `warning: Model "nope" is not in the catalog. Keeping fixture.`)
	if h.app.eng.Model() != "fixture" {
		t.Fatalf("model=%s", h.app.eng.Model())
	}
	h.input("/model gpt-6-luna")
	if h.app.eng.Model() != "gpt-6-luna" {
		t.Fatalf("model=%s", h.app.eng.Model())
	}
	first := h.app.eng.Session().ID
	h.input("/new")
	if h.app.eng.Session().ID == first || h.ui.resets != 1 {
		t.Fatalf("session=%s resets=%d", h.app.eng.Session().ID, h.ui.resets)
	}
	h.input("/bogus")
	mustHave(t, h.ui.text(), "Unknown command /bogus.")
	h.input("/sessions")
	mustHave(t, h.ui.text(), "There are no saved sessions for this workspace.")
	h.input("/quit")
	select {
	case <-h.ui.quit:
	case <-time.After(time.Second):
		t.Fatal("/quit did not quit")
	}
}

// A finished bash call is followed by the diff of the working tree.
func TestDiffShownAfterBash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	h := newHarness(t,
		callTool("c1", "bash", `{"command":"echo new > a.txt"}`),
		say("Changed."),
	)
	dir := h.dir
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.input("change it")
	var diff Block
	for _, b := range h.ui.blocks() {
		if b.Kind == KindDiff {
			diff = b
		}
	}
	text := strings.Join(diff.Lines, "\n")
	if !strings.Contains(text, "a.txt | 2 +-") || !strings.Contains(text, "-old") ||
		!strings.Contains(text, "+new") {
		t.Fatalf("diff block=%q", text)
	}

	// The diff shows again after a reopen.
	h.app.eng.Close()
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	found := false
	for _, b := range h.ui.blocks() {
		if b.Kind == KindDiff &&
			strings.Contains(strings.Join(b.Lines, "\n"), "+new") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the reopened scrollback lacks the diff:\n%s", h.ui.text())
	}
}

func TestDiffIsCappedAt40Lines(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	file := filepath.Join(dir, "big.txt")
	os.WriteFile(file, []byte("x\n"), 0o600)
	run("add", "big.txt")
	run("commit", "-q", "-m", "init")
	os.WriteFile(file, []byte(strings.Repeat("line\n", 200)), 0o600)
	b, ok := gitDiffBlock(t.Context(), dir, 1)
	if !ok || len(b.Lines) != maxDiffLines+1 ||
		!strings.HasPrefix(b.Lines[maxDiffLines], "... ") {
		t.Fatalf("ok=%v lines=%d", ok, len(b.Lines))
	}
	// A clean tree and a non-repository give no block.
	run("checkout", "-q", "--", "big.txt")
	if _, ok := gitDiffBlock(t.Context(), dir, 2); ok {
		t.Fatal("diff block for a clean tree")
	}
	if _, ok := gitDiffBlock(t.Context(), t.TempDir(), 3); ok {
		t.Fatal("diff block outside a repository")
	}
}

// rejectWith fails a model call as the provider rejecting it.
func rejectWith(status int, category llm.ErrorCategory) reply {
	return func(context.Context, llm.Call) (llm.Result, error) {
		return llm.Result{}, &llm.Error{
			Provider:   "test",
			Category:   category,
			SendStatus: llm.SendStatusProviderRejected,
			Status:     status,
		}
	}
}

// A rate-limited turn fails, and Enter retries it, in the same process and
// after a restart.
func TestEnterRetriesFailedTurn(t *testing.T) {
	h := newHarness(t,
		rejectWith(429, llm.ErrorRateLimited),
		rejectWith(529, llm.ErrorOverloaded),
		say("Recovered."),
	)
	h.input("hello")
	mustHave(t, h.ui.text(), "Press Enter to retry, or type a new prompt.")
	last := h.app.eng.Last(t.Context())
	if !last.CanGoOn() || last.View.Stop.Kind != dashi.StopFailed {
		t.Fatalf("stop=%+v next=%s", last.View.Stop, last.View.Next())
	}
	h.input("")
	if n := strings.Count(h.ui.text(), "Press Enter to retry"); n != 2 {
		t.Fatalf("retry hint shown %d times, want 2:\n%s", n, h.ui.text())
	}
	h.app.eng.Close()

	// A new process opens the failed session and retries it.
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	mustHave(t, h.ui.text(), "Turn 1 stopped (failed: ")
	mustHave(t, h.ui.text(), "Press Enter to retry it, or type a new prompt to cancel it.")
	h.input("")
	mustHave(t, h.ui.text(), "Recovered.")
	last = h.app.eng.Last(t.Context())
	if last.CanGoOn() || last.View.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v", last.View.Stop)
	}
	if h.turns() != 1 {
		t.Fatalf("turns=%d", h.turns())
	}
}

// A refusal is not a failure to retry: Enter does not ask the model again.
func TestEnterDoesNotRetryRefusal(t *testing.T) {
	h := newHarness(t, func(context.Context, llm.Call) (llm.Result, error) {
		return llm.Result{
			Message: llm.AssistantMessage(llm.Text("No.")),
			Stop:    llm.Stop{Kind: llm.StopRefusal},
		}, nil
	})
	h.input("hello")
	last := h.app.eng.Last(t.Context())
	if last.View.Stop.Kind != dashi.StopRefused || last.CanGoOn() {
		t.Fatalf("stop=%+v cango=%t", last.View.Stop, last.CanGoOn())
	}
	if strings.Contains(h.ui.text(), "retry") {
		t.Fatalf("a refusal offered a retry:\n%s", h.ui.text())
	}
	h.input("")
	if h.client.sends != 1 {
		t.Fatalf("sends=%d, want 1", h.client.sends)
	}
}

// A damaged last turn does not stop the session (FR-4): a new prompt warns
// that the turn was left out and runs the next turn.
func TestDamagedLastTurnIsSkipped(t *testing.T) {
	h := newHarness(t, say("First answer."), say("Second answer."), say("Third."))
	h.input("first question")
	h.input("second question")
	rec, err := h.store.Recorder(h.app.eng.Session())
	if err != nil {
		t.Fatal(err)
	}
	bad := session.TurnID(h.app.eng.Session().ID, 2)
	if err := os.WriteFile(rec.Path(bad), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.app.eng.Close()

	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	h.input("third question")
	text := h.ui.text()
	mustHave(t, text, "-2 was left out of the history, and the next turn starts after it")
	mustHave(t, text, "Third.")
	if n := turnsOf(t, h.store, h.app.eng.Session()); n != 3 {
		t.Fatalf("turns=%d", n)
	}
	var sent []string
	for _, m := range h.client.inputAt(2).Messages {
		if text := m.Text(); text != "" {
			sent = append(sent, text)
		}
	}
	if got := strings.Join(sent, "|"); got != "first question|First answer.|third question" {
		t.Fatalf("the model saw %q", got)
	}
}

// lockDirAfterStart commits steps, and after turn_started makes the
// session directory read-only, so the session file cannot be saved while
// the turn log, in its own directory, still records.
type lockDirAfterStart struct {
	dashi.Recorder
	root string
}

func (r *lockDirAfterStart) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == dashi.StepTurnStarted {
		id := req.Step.TurnID[:strings.LastIndex(req.Step.TurnID, "-")]
		return os.Chmod(filepath.Join(r.root, id), 0o500)
	}
	return nil
}

// A session file that fails to save after the first turn started leaves
// the turn log in place when the session closes.
func TestFailedSessionSaveKeepsTurnLog(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs directory permissions")
	}
	h := newHarness(t, say("Done."))
	h.app.eng.Close()
	h.recorder = func(rec *filerecorder.Recorder) dashi.Recorder {
		return &lockDirAfterStart{Recorder: rec, root: h.store.Root()}
	}
	h.build(Options{})
	s := h.app.eng.Session()
	dir := filepath.Join(h.store.Root(), s.ID)
	h.input("save me")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustHave(t, h.ui.text(), "The session file was not saved")
	if h.store.Saved(s) {
		t.Fatal("the session file was saved")
	}
	h.app.eng.Close()
	if n := turnsOf(t, h.store, s); n != 1 {
		t.Fatalf("turns=%d after close", n)
	}
}
