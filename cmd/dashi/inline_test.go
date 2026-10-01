package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

func TestInlineCompletionPrintsOnceAndResetStartsNewSegment(t *testing.T) {
	var output bytes.Buffer
	i := newInlineScreen(newScreen("fixture"), &output)
	flush := func() { i.HandleEvent(flushEvent{}) }
	i.Emit(Block{Kind: KindAssistant, Done: true}) // Invisible finished tool-only assistant.
	i.Emit(Block{ID: "user", Kind: KindUser, Text: "REOPENED USER", Done: true})
	i.Emit(Block{ID: "assistant", Kind: KindAssistant, Text: "streaming"})
	flush()
	if strings.Contains(output.String(), "streaming") {
		t.Fatal("unfinished text entered native scrollback")
	}
	i.Emit(Block{ID: "assistant", Kind: KindAssistant, Text: "COMPLETED ANSWER", Done: true})
	flush()
	flush()
	if strings.Count(output.String(), "COMPLETED ANSWER") != 1 || strings.Count(output.String(), "REOPENED USER") != 1 {
		t.Fatalf("completed output duplicated: %q", output.String())
	}
	i.Reset()
	i.Emit(Block{Kind: KindNotice, Text: "NEW SESSION", Done: true})
	flush()
	if strings.Count(output.String(), "NEW SESSION") != 1 || strings.Count(output.String(), "REOPENED USER") != 1 {
		t.Fatal("reset reprinted the old segment")
	}
	output.Reset()
	i.input = "kept draft"
	i.Emit(Block{Kind: KindTool, Name: "read", Detail: "first\nFINAL RETAINED LINE", Lines: []string{"first"}, Done: true})
	flush()
	i.showScrollback(true)
	if !strings.Contains(output.String(), "FINAL RETAINED LINE") || i.input != "kept draft" {
		t.Fatal("full inspection lost output or draft")
	}
	i.HandleEvent(tui.KeyEvent{Key: tui.KeyEnter})
	if i.inspecting || i.input != "kept draft" {
		t.Fatal("pager Enter became a submitted draft")
	}
}

type inlineProbe struct{ reply chan inlineState }

func (inlineProbe) Timestamp() time.Time { return time.Now() }

type inlineState struct {
	input                          string
	mode                           Mode
	approval, question, inspecting bool
	printed                        int
}
type runtimeInline struct {
	*inlineScreen
	ready chan struct{}
	once  sync.Once
}

func (i *runtimeInline) LiveView() tui.View {
	v := i.inlineScreen.LiveView()
	i.once.Do(func() { close(i.ready) })
	return v
}
func (i *runtimeInline) HandleEvent(e tui.Event) []tui.Cmd {
	if p, ok := e.(inlineProbe); ok {
		p.reply <- inlineState{input: i.input, mode: i.ctrl.Mode(), approval: i.approval != nil, question: i.asking != nil, inspecting: i.inspecting, printed: i.printed}
		return nil
	}
	return i.inlineScreen.HandleEvent(e)
}

// Run with a PTY: go test -run TestInlineRuntime -v. The ordinary suite keeps
// all keyless unit/runtime tests; this also exercises InlineApp's raw-mode
// lifecycle, focused input, and actual event loop without provider calls.
func TestInlineRuntimeApprovalsQuestionsAndRecovery(t *testing.T) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("requires a terminal for Wonton InlineApp")
	}
	started := make(chan struct{})
	cfg := testConfig(t, callTool("write", workspace.ToolWriteFile, `{"path":"inline.txt","content":"saved"}`), callTool("ask", workspace.ToolAskUser, `{"question":"Choose","multiple":true,"choices":[{"label":"one"},{"label":"two"}]}`), blockUntilDone(started), say("Recovered inline."))
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	i := &runtimeInline{inlineScreen: newInlineScreen(s, io.Discard), ready: make(chan struct{})}
	i.runner = tui.NewInlineApp(tui.WithInlineInput(strings.NewReader("")), tui.WithInlineOutput(io.Discard), tui.WithInlineWidth(80), tui.WithInlineFPS(60), tui.WithInlineBracketedPaste(true))
	s.send = i.runner.SendEvent
	done := make(chan error, 1)
	go func() { done <- i.runner.Run(i) }()
	t.Cleanup(func() {
		a.ctrl.CancelWork()
		a.ctrl.Wait()
		i.runner.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("inline runtime did not stop")
		}
		a.eng.Close()
	})
	select {
	case <-i.ready:
	case err := <-done:
		t.Fatalf("inline startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("inline did not render")
	}
	key := func(k tui.KeyEvent) { i.runner.SendEvent(k) }
	typeText := func(text string) {
		for _, r := range text {
			key(tui.KeyEvent{Rune: r})
		}
	}
	wait := func(want func(inlineState) bool) inlineState {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		var last inlineState
		for {
			p := inlineProbe{reply: make(chan inlineState, 1)}
			i.runner.SendEvent(p)
			select {
			case last = <-p.reply:
				if want(last) {
					return last
				}
			case <-deadline.C:
				t.Fatalf("inline timeout: %+v", last)
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatalf("inline timeout: %+v", last)
			}
		}
	}
	typeText("write and ask")
	key(tui.KeyEvent{Key: tui.KeyEnter})
	wait(func(s inlineState) bool { return s.approval })
	key(tui.KeyEvent{Rune: 'y'})
	wait(func(s inlineState) bool { return s.question })
	key(tui.KeyEvent{Rune: ' '})
	key(tui.KeyEvent{Key: tui.KeyArrowDown})
	key(tui.KeyEvent{Rune: ' '})
	key(tui.KeyEvent{Key: tui.KeyTab})
	key(tui.KeyEvent{Key: tui.KeyEnter})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("inline answer did not dispatch")
	}
	typeText("kept draft")
	wait(func(s inlineState) bool { return s.input == "kept draft" })
	key(tui.KeyEvent{Key: tui.KeyCtrlC})
	wait(func(s inlineState) bool { return s.mode == ModeIdle && s.input == "kept draft" })
	a.ctrl.Input("")
	a.ctrl.Wait()
	before := wait(func(s inlineState) bool { return s.mode == ModeIdle && s.printed > 0 })
	a.ctrl.Input("/scrollback raw")
	wait(func(s inlineState) bool { return s.inspecting })
	key(tui.KeyEvent{Key: tui.KeyEnter})
	after := wait(func(s inlineState) bool { return !s.inspecting && s.input == "kept draft" })
	if after.printed != before.printed {
		t.Fatal("inspection reprinted the completed prefix as new work")
	}
}

// The prompt has already reached native scrollback when preparation records
// its media warning. The warning must be visible during the live turn,
// before an answer.
func TestInlineMediaWarningReachesScrollbackOnce(t *testing.T) {
	var output bytes.Buffer
	i := newInlineScreen(newScreen("fixture"), &output)
	r := NewRenderer(1, "fixture", "")
	emit := func(step dashi.Step) {
		for _, block := range r.Event(stepEvent(step)) {
			i.Emit(block)
		}
		i.HandleEvent(flushEvent{})
	}
	emit(dashi.Step{Kind: dashi.StepTurnStarted, TurnID: "turn", Started: &dashi.StartFact{
		Agent: dashi.Agent{ActiveCompaction: &dashi.ActiveCompactionPolicy{}}, ActiveCompactionWindow: 128000,
	}})
	if strings.Contains(output.String(), "text-only") {
		t.Fatal("per-Turn protection notice reached native scrollback")
	}
	// The model_call_started step records the warning before the reply
	// streams. A re-delivered warning updates the same row.
	warn := dashi.Step{Kind: dashi.StepModelCallStarted, TurnID: "turn", Model: &dashi.ModelFact{ModelCallID: "answer"}, Warnings: []dashi.Warning{{Code: dashi.WarningActiveCompactionMediaEstimateUnavailable}}}
	emit(warn)
	emit(warn)
	for _, block := range r.Event(dashi.Event{ModelCallID: "answer", Delta: &llm.Delta{Kind: llm.DeltaText, Text: "unfinished answer"}}) {
		i.Emit(block)
	}
	i.HandleEvent(flushEvent{})
	if count := strings.Count(output.String(), "Active-Turn protection unavailable for media"); count != 1 {
		t.Fatalf("recorded warning was hidden or repeated in native scrollback: count=%d output=%q", count, output.String())
	}
	if strings.Contains(output.String(), "unfinished answer") {
		t.Fatal("warning update flushed unfinished answer")
	}
	warnings := 0
	for _, block := range i.tr.Blocks {
		if block.State == "unavailable" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatal("warning update duplicated authoritative transcript row")
	}
	i.Reset()
	for _, block := range r.Event(stepEvent(warn)) {
		i.Emit(block)
	}
	i.HandleEvent(flushEvent{})
	if count := strings.Count(output.String(), "Active-Turn protection unavailable for media"); count != 2 {
		t.Fatal("session reset suppressed recorded warning")
	}
}
