package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

type transcriptCapture struct {
	out  io.Writer
	copy func(context.Context, string) (string, error)
}

func (transcriptCapture) Timestamp() time.Time { return time.Now() }

type eventRuntimeInput struct {
	events chan tui.Event
	done   chan struct{}
}

func (i *eventRuntimeInput) ReadEvent() (tui.Event, error) {
	select {
	case e := <-i.events:
		return e, nil
	case <-i.done:
		return nil, io.EOF
	}
}
func (*eventRuntimeInput) SetPasteTabWidth(int) {}

type transcriptProbe struct{ reply chan transcriptState }

func (transcriptProbe) Timestamp() time.Time { return time.Now() }

type transcriptState struct {
	text, input, selected                           string
	follow, approval, question, own, denying, mouse bool
	mode                                            Mode
}
type transcriptRuntime struct {
	*screen
	rt    *tui.Runtime
	ready chan struct{}
	once  sync.Once
}

func (s *transcriptRuntime) SetRuntime(rt *tui.Runtime) { s.rt = rt; s.screen.SetRuntime(rt) }
func (s *transcriptRuntime) View() tui.View {
	v := s.screen.View()
	s.once.Do(func() { close(s.ready) })
	return v
}
func (s *transcriptRuntime) HandleEvent(e tui.Event) []tui.Cmd {
	if capture, ok := e.(transcriptCapture); ok {
		if capture.out != nil {
			s.clipboardOut = capture.out
		}
		if capture.copy != nil {
			s.clipboardWrite = capture.copy
		}
		return nil
	}
	if p, ok := e.(transcriptProbe); ok {
		var b strings.Builder
		for y := range 24 {
			for x := range 80 {
				cell := s.rt.Terminal().Cell(x, y)
				if cell.Continuation {
					continue
				}
				char := cell.Char
				if char == 0 {
					char = ' '
				}
				b.WriteRune(char)
				b.WriteString(cell.Trailing)
			}
			b.WriteByte('\n')
		}
		p.reply <- transcriptState{text: b.String(), input: s.input, selected: s.vp.SelectedText(), follow: s.vp.Follow, mode: s.ctrl.Mode(), approval: s.approval != nil, question: s.asking != nil, own: s.asking != nil && s.asking.own, denying: s.approval != nil && s.approval.denying, mouse: s.mouseEnabled}
		return nil
	}
	return s.screen.HandleEvent(e)
}

type runtimeFixture struct {
	t      *testing.T
	scr    *transcriptRuntime
	app    *app
	frames []string
	input  *eventRuntimeInput
}

func newTranscriptRuntime(t *testing.T, script ...reply) *runtimeFixture {
	t.Helper()
	return newTranscriptRuntimeConfig(t, testConfig(t, script...))
}

func newTranscriptRuntimeConfig(t *testing.T, cfg appConfig) *runtimeFixture {
	t.Helper()
	s := &transcriptRuntime{screen: newScreen(cfg.Dir), ready: make(chan struct{})}
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	rt := tui.NewRuntime(tui.NewTestTerminal(80, 24, io.Discard), s, 60)
	input := &eventRuntimeInput{events: make(chan tui.Event, 20), done: make(chan struct{})}
	rt.SetInputSource(input)
	done := make(chan error, 1)
	go func() { done <- rt.Run() }()
	t.Cleanup(func() {
		a.ctrl.CancelWork()
		a.ctrl.Wait()
		rt.Stop()
		close(input.done)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop")
		}
		a.eng.Close()
		if a.wiring.Close != nil {
			_ = a.wiring.Close()
		}
	})
	select {
	case <-s.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not render")
	}
	fixture := &runtimeFixture{t: t, scr: s, app: a, input: input}
	t.Cleanup(func() { fixture.exportFrames() })
	return fixture
}
func (h *runtimeFixture) key(k tui.KeyEvent) { h.scr.rt.SendEvent(k) }
func (h *runtimeFixture) typeText(text string) {
	for _, r := range text {
		h.key(tui.KeyEvent{Rune: r})
	}
}
func (h *runtimeFixture) wait(want func(transcriptState) bool) transcriptState {
	h.t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var last transcriptState
	for {
		p := transcriptProbe{reply: make(chan transcriptState, 1)}
		h.scr.rt.SendEvent(p)
		select {
		case last = <-p.reply:
			if want(last) {
				h.frames = append(h.frames, last.text)
				return last
			}
		case <-deadline.C:
			h.t.Fatalf("runtime state timed out:\n%+v", last)
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			h.t.Fatalf("runtime state timed out:\n%+v", last)
		}
	}
}

func TestRuntimeTranscriptNavigationPreservesDraftAndSelection(t *testing.T) {
	h := newTranscriptRuntime(t)
	var source strings.Builder
	source.WriteString("```text\n")
	for i := range 200 {
		fmt.Fprintf(&source, "row %03d 世界\n", i)
	}
	source.WriteString("```")
	h.scr.Emit(Block{ID: "stream", Kind: KindAssistant, Text: source.String()})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "row 199") })
	h.typeText("draft 世界")
	h.wait(func(s transcriptState) bool { return s.input == "draft 世界" })
	h.key(tui.KeyEvent{Key: tui.KeyPageUp})
	before := h.wait(func(s transcriptState) bool { return !s.follow && strings.Contains(s.text, "lines below") })
	h.scr.Emit(Block{ID: "stream", Kind: KindAssistant, Text: source.String() + "\nnew streamed output"})
	after := h.wait(func(s transcriptState) bool {
		return !s.follow && s.input == "draft 世界" && strings.Contains(s.text, "lines below")
	})
	if strings.Contains(after.text, "new streamed output") {
		t.Fatal("streaming forced the reader to the bottom")
	}
	h.key(tui.KeyEvent{Key: tui.KeyHome, Ctrl: true})
	top := h.wait(func(s transcriptState) bool { return !s.follow && strings.Contains(s.text, "row 000") })
	row := -1
	for y, line := range strings.Split(top.text, "\n") {
		if strings.Contains(line, "row 000") {
			row = y
			break
		}
	}
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: 8, Y: row})
	selected := h.wait(func(s transcriptState) bool { return strings.Contains(s.selected, "世界") })
	if !utf8.ValidString(selected.selected) || selected.input != before.input {
		t.Fatal("selection damaged Unicode or draft")
	}
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.selected == "" && s.input == "draft 世界" && !s.follow })
	h.key(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	h.wait(func(s transcriptState) bool { return s.follow && s.input == "draft 世界" })
	h.app.ctrl.Input("/mouse")
	h.wait(func(s transcriptState) bool { return !s.mouse })
}

func TestRuntimeQuestionCheckboxesAndDraftRestoration(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := newTranscriptRuntime(t, hold(started, release, callTool("ask", workspace.ToolAskUser, `{"question":"Which paths?","multiple":true,"choices":[{"label":"a,b","description":"first path"},{"label":"世界\nnext","description":"second path"}]}`)), say("Selected."))
	h.typeText("choose paths")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	<-started
	h.typeText("saved draft")
	h.wait(func(s transcriptState) bool { return s.input == "saved draft" })
	close(release)
	h.wait(func(s transcriptState) bool { return s.question && strings.Contains(s.text, "Question pending") })
	h.key(tui.KeyEvent{Rune: ' '})
	h.key(tui.KeyEvent{Key: tui.KeyArrowDown})
	h.key(tui.KeyEvent{Rune: ' '})
	h.key(tui.KeyEvent{Key: tui.KeyTab})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Submit selected") })
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return !s.question && s.mode == ModeIdle && s.input == "saved draft" })
	rec, err := h.app.ctrl.store.Recorder(h.app.eng.Session())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := rec.Load(t.Context(), session.TurnID(h.app.eng.Session().ID, 1))
	if err != nil {
		t.Fatal(err)
	}
	var output string
	for _, step := range turn.Steps {
		if step.Tool != nil && step.Tool.Call.ID == "ask" {
			output = outputText(step.Tool.Outcome)
		}
	}
	if output != `{"selections":["a,b","世界\nnext"]}` {
		t.Fatalf("wrong model answer: %q", output)
	}
}

func TestRuntimeApprovalFullPreviewDenyReasonAndEscape(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	content := strings.Repeat("a line of proposed code\n", 60) + "END OF COMPLETE DIFF\n"
	args := fmt.Sprintf(`{"path":"long.txt","content":%q}`, content)
	h := newTranscriptRuntime(t, hold(started, release, callTool("write", workspace.ToolWriteFile, args)), say("Denied."), callTool("again", workspace.ToolWriteFile, args))
	h.typeText("write")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	<-started
	h.typeText("saved draft")
	h.wait(func(s transcriptState) bool { return s.input == "saved draft" })
	close(release)
	h.wait(func(s transcriptState) bool {
		return s.approval && strings.Contains(s.text, "Allow for this Dashi run")
	})
	h.key(tui.KeyEvent{Key: tui.KeyEnd})
	h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "END OF COMPLETE DIFF") && strings.Contains(s.text, "Allow once")
	})
	h.key(tui.KeyEvent{Rune: 'n'})
	h.wait(func(s transcriptState) bool { return s.denying && strings.Contains(s.text, "Reason:") })
	h.typeText("Please use a patch")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return !s.approval && s.input == "saved draft" && s.mode == ModeIdle })
	var reason string
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "write" {
			reason = step.Tool.Reason
		}
	}
	if reason != "Please use a patch" {
		t.Fatalf("denial reason lost: %q", reason)
	}
	if _, err := os.Stat(filepath.Join(h.app.ctrl.dir, "long.txt")); !os.IsNotExist(err) {
		t.Fatal("denied write dispatched")
	}
	h.app.ctrl.Input("again")
	h.wait(func(s transcriptState) bool { return s.approval })
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return !s.approval && s.mode == ModeIdle && s.input == "saved draft" })
}

func (h *runtimeFixture) turn(n int) dashi.Turn {
	h.t.Helper()
	rec, err := h.app.ctrl.store.Recorder(h.app.eng.Session())
	if err != nil {
		h.t.Fatal(err)
	}
	turn, err := rec.Load(h.t.Context(), session.TurnID(h.app.eng.Session().ID, n))
	if err != nil {
		h.t.Fatal(err)
	}
	return turn
}
func TestRuntimeOwnWordsAndCanceledQuestion(t *testing.T) {
	q := `{"question":"Choose or explain","multiple":true,"choices":[{"label":"a"},{"label":"b"}]}`
	h := newTranscriptRuntime(t, callTool("own", workspace.ToolAskUser, q), say("Accepted."), callTool("cancel", workspace.ToolAskUser, q))
	h.typeText("choose")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.question })
	h.key(tui.KeyEvent{Rune: 'o'})
	h.wait(func(s transcriptState) bool { return s.own && strings.Contains(s.text, "Answer:") })
	h.typeText("my own answer")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return !s.question && s.mode == ModeIdle })
	var answer string
	for _, step := range h.turn(1).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "own" {
			answer = outputText(step.Tool.Outcome)
		}
	}
	if answer != `{"text":"my own answer"}` {
		t.Fatalf("wrong own-words answer: %s", answer)
	}
	h.typeText("new draft")
	h.wait(func(s transcriptState) bool { return s.input == "new draft" })
	h.app.ctrl.Input("ask again")
	h.wait(func(s transcriptState) bool { return s.question })
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool { return !s.question && s.mode == ModeIdle && s.input == "new draft" })
	if stop := h.app.eng.Last(t.Context()).View.Stop; stop.Kind != dashi.StopInterrupted {
		t.Fatalf("cancellation became %s", stop.Kind)
	}
	for _, step := range h.turn(2).Steps {
		if step.Tool != nil && step.Tool.Call.ID == "cancel" && step.Tool.Outcome != nil && outputText(step.Tool.Outcome) != "" {
			t.Fatal("canceled question fabricated an answer")
		}
	}
}

func TestRuntimeFocusedCtrlCInterruptsOnce(t *testing.T) {
	started := make(chan struct{})
	h := newTranscriptRuntime(t, blockUntilDone(started), say("Second completed."))
	h.typeText("first")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	<-started
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "Interrupted") })
	h.typeText("second")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Second completed.")
	})
}

func TestRuntimeRememberedWritesAcrossNewAndReopenedSessions(t *testing.T) {
	h := newTranscriptRuntime(t, callTool("w1", workspace.ToolWriteFile, `{"path":"one.txt","content":"one"}`), say("First saved."), callTool("w2", workspace.ToolWriteFile, `{"path":"two.txt","content":"two"}`), say("Second saved."), callTool("w3", workspace.ToolWriteFile, `{"path":"three.txt","content":"three"}`), say("Third saved."))
	firstID := h.app.eng.Session().ID
	h.typeText("first")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return s.approval })
	h.key(tui.KeyEvent{Key: tui.KeyHome})
	h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "covers all write_file calls in this workspace")
	})
	h.key(tui.KeyEvent{Rune: 'a'})
	h.app.ctrl.Wait()
	h.app.ctrl.Input("/new")
	h.app.ctrl.Wait()
	h.app.ctrl.Input("second")
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return !s.approval && s.mode == ModeIdle && strings.Contains(s.text, "Second saved.")
	})
	h.app.ctrl.openSession(firstID)
	h.app.ctrl.Input("third")
	h.app.ctrl.Wait()
	h.wait(func(s transcriptState) bool {
		return !s.approval && s.mode == ModeIdle && strings.Contains(s.text, "Third saved.")
	})
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if _, err := os.Stat(filepath.Join(h.app.ctrl.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeRetainsCompleteToolOutputAndExpands(t *testing.T) {
	h := newTranscriptRuntime(t, callTool("read", workspace.ToolReadFile, `{"path":"output.txt"}`), say("Read."))
	content := strings.Repeat("tool output line 世界\n", 75) + "END OF COMPLETE TOOL OUTPUT\x1b[2J\n"
	if err := os.WriteFile(filepath.Join(h.app.ctrl.dir, "output.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	h.typeText("read")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.app.ctrl.Wait()
	shown := h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "click to expand") })
	row := -1
	for y, line := range strings.Split(shown.text, "\n") {
		if strings.Contains(line, "read_file") {
			row = y
			break
		}
	}
	if row < 0 {
		t.Fatalf("missing clickable header:\n%s", shown.text)
	}
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 1, X: 3, Y: row})
	h.key(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "END OF COMPLETE TOOL OUTPUT�[2J") })
}

func (h *runtimeFixture) exportFrames() {
	dir := os.Getenv("DASHI_TRANSCRIPT_EVIDENCE")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		h.t.Error(err)
		return
	}
	var cast strings.Builder
	cast.WriteString("{\"version\":2,\"width\":80,\"height\":24,\"title\":\"Dashi Wonton keyless runtime checkpoints (slowed 3s)\"}\n")
	for i, frame := range h.frames {
		row, _ := json.Marshal([]any{i * 3, "o", "\x1b[2J\x1b[H" + frame})
		cast.Write(row)
		cast.WriteByte('\n')
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%02d.txt", strings.ReplaceAll(h.t.Name(), "/", "_"), i)), []byte(frame), 0600); err != nil {
			h.t.Error(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(h.t.Name(), "/", "_")+".cast"), []byte(cast.String()), 0600); err != nil {
		h.t.Error(err)
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// This fixture is finite memory; it cannot block on external output.
func (b *synchronizedBuffer) SetWriteDeadline(time.Time) error { return nil }

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}
func (b *synchronizedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
func TestRuntimeNativeScrollbackReturnsWithDraftAndSafeOutput(t *testing.T) {
	for _, mode := range []string{"raw", ""} {
		t.Run(mode, func(t *testing.T) {
			h := newTranscriptRuntime(t)
			h.scr.Emit(Block{Kind: KindAssistant, Text: "VISIBLE TEXT 世界\x1b[2J", Done: true})
			h.scr.Emit(Block{Kind: KindTool, Name: "read", Detail: "first\nLAST FULL LINE", Done: true})
			h.typeText("kept draft")
			h.wait(func(s transcriptState) bool { return s.input == "kept draft" })
			var output synchronizedBuffer
			h.scr.rt.SendEvent(transcriptCapture{out: &output})
			h.app.ctrl.Input(strings.TrimSpace("/scrollback " + mode))
			deadline := time.Now().Add(3 * time.Second)
			for !strings.Contains(output.text(), "Press Enter or Escape") {
				if time.Now().After(deadline) {
					t.Fatalf("scrollback did not open: %q", output.text())
				}
				time.Sleep(time.Millisecond)
			}
			text := output.text()
			if !strings.Contains(text, "LAST FULL LINE") || !strings.Contains(text, "␛[2J") || strings.Contains(text, "世界\x1b[2J") {
				t.Fatalf("lost output or executable control: %q", text)
			}
			if mode == "" && !strings.Contains(text, "\x1b[") {
				t.Fatal("generated styling was escaped instead of rendered")
			}
			h.input.events <- tui.KeyEvent{Key: tui.KeyEnter}
			h.wait(func(s transcriptState) bool {
				return s.input == "kept draft" && strings.Contains(s.text, "❯ kept draft")
			})
		})
	}
}
