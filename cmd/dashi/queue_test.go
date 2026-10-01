package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

// hold makes a reply wait until release is closed, and closes started when
// it begins.
func hold(started, release chan struct{}, r reply) reply {
	return func(ctx context.Context, call llm.Call) (llm.Result, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Result{}, ctx.Err()
		}
		return r(ctx, call)
	}
}

// A prompt queued with /queue while a turn runs becomes the next turn when
// the running one ends, in the same run.
func TestQueuedPromptRunsAfterTheTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t,
		hold(started, release, say("First done.")),
		say("Tests pass."),
	)
	h.app.ctrl.Input("fix the bug")
	<-started
	h.app.ctrl.Input("/queue run the tests")
	mustHave(t, h.ui.text(), "queued for the next turn: run the tests")
	close(release)
	h.app.ctrl.Wait()
	text := h.ui.text()
	mustHave(t, text, "First done.")
	mustHave(t, text, "> run the tests")
	mustHave(t, text, "Tests pass.")
	mustHave(t, text, "turn 2 · fixture")
	if n := h.turns(); n != 2 {
		t.Fatalf("turns=%d", n)
	}
	if got := inputText(h.client.inputAt(1)); !strings.Contains(got, "First done.") {
		t.Fatalf("turn 2 lacks turn 1's history:\n%s", got)
	}
}

// A steer that arrives when the turn can make no further model call is not
// lost: it becomes the next turn.
func TestMissedSteerBecomesTheNextTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := newHarnessLimit(t, 1,
		hold(started, release, say("Done.")),
		say("Also done."),
	)
	h.app.ctrl.Input("do it")
	<-started
	h.app.ctrl.Input("and update the docs")
	mustHave(t, h.ui.text(), "steer queued: and update the docs")
	close(release)
	h.app.ctrl.Wait()
	text := h.ui.text()
	mustHave(t, text, "> and update the docs")
	mustHave(t, text, "Also done.")
	if n := h.turns(); n != 2 {
		t.Fatalf("turns=%d", n)
	}
}

// A prompt queued before an interruption goes ahead of the next prompt.
func TestQueuedPromptJoinsTheNextPrompt(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started), say("Both done."))
	h.app.ctrl.Input("first")
	<-started
	h.app.ctrl.Input("/queue then this")
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "1 queued message will go with the next prompt.")
	h.input("second")
	mustHave(t, h.ui.text(), "Turn 1 canceled.")
	mustHave(t, h.ui.text(), "> then this\nsecond")
	if n := h.turns(); n != 2 {
		t.Fatalf("turns=%d", n)
	}
}

// A queued prompt that no turn took is shown after /discard, and /new,
// /quit, and Ctrl-C warn before they drop it; repeating them goes ahead.
func TestQueuedPromptIsNotDroppedSilently(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started))
	h.app.ctrl.Input("first")
	<-started
	h.app.ctrl.Input("/queue then this")
	h.app.ctrl.Interrupt()
	h.app.ctrl.Wait()
	h.input("/discard")
	mustHave(t, h.ui.text(), "Turn 1 canceled.")
	const waits = "1 queued message will go with the next prompt."
	if n := strings.Count(h.ui.text(), waits); n != 2 {
		t.Fatalf("%q shown %d times, want after the interrupt and the discard:\n%s",
			waits, n, h.ui.text())
	}

	const lost = "1 queued message will be lost. Repeat %s to go ahead."
	if h.app.ctrl.Interrupt() {
		t.Fatal("Ctrl-C quit with a queued prompt")
	}
	mustHave(t, h.ui.text(), fmt.Sprintf(lost, "Ctrl-C"))
	h.input("/quit")
	mustHave(t, h.ui.text(), fmt.Sprintf(lost, "/quit"))
	h.input("/new")
	mustHave(t, h.ui.text(), fmt.Sprintf(lost, "/new"))
	first := h.app.eng.Session().ID
	h.input("/new")
	if h.app.eng.Session().ID == first {
		t.Fatal("the repeated /new did not start a new session")
	}
	if h.app.ctrl.Interrupt() != true {
		t.Fatal("Ctrl-C did not quit an idle session with nothing queued")
	}
}

// crashAfterToolStart records steps until a tool call starts, then fails
// every commit, as a process that exited would.
type crashAfterToolStart struct {
	*filerecorder.Recorder
	dead atomic.Bool
}

func (r *crashAfterToolStart) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if r.dead.Load() {
		return errors.New("the process exited")
	}
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == dashi.StepToolStarted {
		r.dead.Store(true)
	}
	return nil
}

// crashInCommand runs a prompt whose write_note call starts, then the
// process exits before its result is recorded. It reopens the session as a
// new process would and returns the harness.
func crashInCommand(t *testing.T, runs *atomicCounter, script ...reply) *harness {
	t.Helper()
	withNoteTool(t, runs)
	h := newHarness(t, append([]reply{
		callTool("c1", "write_note", `{"text":"deploy"}`),
	}, script...)...)
	h.app.eng.Close()
	h.recorder = func(rec *filerecorder.Recorder) dashi.Recorder {
		return &crashAfterToolStart{Recorder: rec}
	}
	h.build(Options{Resume: true})
	h.input("write the deploy note")
	if runs.Load() != 1 {
		t.Fatalf("runs=%d", runs.Load())
	}
	h.app.eng.Close()

	// A new process opens the same session. Its lock says no other dashi
	// runs, but the command may still be running.
	h.recorder = nil
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	h.app.ctrl.Open()
	mustHave(t, h.ui.text(), "Turn 1 stopped while write_note was running (call c1)")
	return h
}

// After a crash in a command, a new prompt refuses and names the call;
// /discard records the cancel, and the prompt then runs.
func TestCrashInCommandThenDiscard(t *testing.T) {
	var runs atomicCounter
	h := crashInCommand(t, &runs, say("Starting over."))
	h.input("start over")
	mustHave(t, h.ui.text(),
		"The last command (call c1 in turn 1) may still be running. "+
			"Type /done once it has finished, or /discard to discard the turn.")
	if n := h.turns(); n != 1 {
		t.Fatalf("turns=%d", n)
	}
	h.input("/discard")
	mustHave(t, h.ui.text(), "Turn 1 canceled.")
	h.input("start over")
	mustHave(t, h.ui.text(), "Starting over.")
	if n := h.turns(); n != 2 || runs.Load() != 1 {
		t.Fatalf("turns=%d runs=%d", n, runs.Load())
	}
	// The record keeps the cancel beside the call that never settled.
	turn := h.turn(1)
	view, err := dashi.Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Stop.Kind != dashi.StopCanceled ||
		view.Stop.Reason != "The user discarded the turn." ||
		view.Tools["c1"].Status == dashi.ToolSettled {
		t.Fatalf("stop=%+v call=%s", view.Stop, view.Tools["c1"].Status)
	}
}

// /done records the command as finished; the next prompt closes the turn
// and runs.
func TestCrashInCommandThenDone(t *testing.T) {
	var runs atomicCounter
	h := crashInCommand(t, &runs, say("Next."))
	h.input("/done")
	h.input("carry on")
	mustHave(t, h.ui.text(), "Next.")
	if n := h.turns(); n != 2 || runs.Load() != 1 {
		t.Fatalf("turns=%d runs=%d", n, runs.Load())
	}
	view, err := dashi.Replay(h.turn(1))
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["c1"].Outcome == nil ||
		!strings.Contains(outputText(view.Tools["c1"].Outcome), "marked it done") {
		t.Fatalf("call=%+v", view.Tools["c1"])
	}
}

// /quit, /exit, and Ctrl-D during a run cancel the turn. When a prompt is
// still queued once the run settles, the program warns instead of quitting,
// and repeating the action quits.
func TestQuitDuringRunKeepsQueuedPrompt(t *testing.T) {
	for _, action := range []string{"/quit", "/exit", "Ctrl-D"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{})
			h := newHarness(t, blockUntilDone(started))
			quit := func() bool {
				select {
				case <-h.ui.quit:
					return true
				default:
					return false
				}
			}
			h.app.ctrl.Input("first")
			<-started
			h.app.ctrl.Input("/queue then this")
			if action == "Ctrl-D" {
				h.app.ctrl.QuitDuringRun(action)
			} else {
				h.app.ctrl.Input(action)
			}
			h.app.ctrl.Wait()
			const lost = "1 queued message will be lost. Repeat %s to go ahead."
			mustHave(t, h.ui.text(), fmt.Sprintf(lost, func() string {
				if action == "/exit" {
					return "/quit"
				}
				return action
			}()))
			if n := strings.Count(h.ui.text(), fmt.Sprintf(lost, func() string {
				if action == "/exit" {
					return "/quit"
				}
				return action
			}())); n != 1 {
				t.Fatalf("warned %d times:\n%s", n, h.ui.text())
			}
			if quit() || h.app.ctrl.Mode() != ModeIdle {
				t.Fatalf("quit=%v mode=%v with a queued prompt", quit(), h.app.ctrl.Mode())
			}
			if n := h.app.eng.Queued(); n != 1 {
				t.Fatalf("queued=%d", n)
			}
			if action == "Ctrl-D" {
				if h.app.ctrl.keepQueued(action) {
					t.Fatal("the repeated Ctrl-D warned again")
				}
			} else {
				h.input(action)
				if !quit() {
					t.Fatalf("the repeated %s did not quit", action)
				}
			}
		})
	}
}

// Ctrl-C during an approval prompt cancels the run but keeps a queued
// prompt. Idle Ctrl-C still uses the existing two-step queue exit guard.
func TestInterruptDuringApprovalKeepsQueuedPrompt(t *testing.T) {
	var runs atomicCounter
	withNoteTool(t, &runs)
	h := newHarness(t, callTool("c1", "write_note", `{"text":"one"}`))
	asked := make(chan struct{})
	h.ui.answer = func(ctx context.Context, _ ApprovalRequest) (Approval, error) {
		close(asked)
		<-ctx.Done()
		return Approval{}, ctx.Err()
	}
	h.app.ctrl.Input("write a note")
	<-asked
	if err := h.app.eng.Queue("then this"); err != nil {
		t.Fatal(err)
	}
	if h.app.ctrl.Interrupt() {
		t.Fatal("Ctrl-C in approval must cancel the run first")
	}
	h.app.ctrl.Wait()
	select {
	case <-h.ui.quit:
		t.Fatalf("quit with a queued prompt:\n%s", h.ui.text())
	default:
	}
	if n := h.app.eng.Queued(); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	if h.app.ctrl.Interrupt() {
		t.Fatal("first idle Ctrl-C lost queued work")
	}
	mustHave(t, h.ui.text(), "1 queued message will be lost. Repeat Ctrl-C to go ahead.")
	if !h.app.ctrl.Interrupt() {
		t.Fatal("repeated idle Ctrl-C did not quit")
	}
}

// With nothing queued, /quit during a run quits once the run settles.
func TestQuitDuringRunQuits(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, blockUntilDone(started))
	h.app.ctrl.Input("first")
	<-started
	h.app.ctrl.Input("/quit")
	h.app.ctrl.Wait()
	select {
	case <-h.ui.quit:
	default:
		t.Fatalf("/quit did not quit:\n%s", h.ui.text())
	}
}
