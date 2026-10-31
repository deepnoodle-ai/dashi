package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

// menuFixture is a screen with one item of each kind the transcript menu
// knows, and a clipboard that records what it is given.
type menuFixture struct {
	*screenHarness
	copied chan string
}

const (
	menuUserText      = "please run the tests"
	menuReplyText     = "The **tests** pass.\n\n```sh\ngo test ./...\n```"
	menuReasoningText = "The user wants the tests run"
)

func newMenuFixture(t *testing.T) *menuFixture {
	t.Helper()
	f := &menuFixture{screenHarness: newScreenHarness(t, 0), copied: make(chan string, 4)}
	s := f.scr
	f.mu.Lock()
	defer f.mu.Unlock()
	s.clipboardWrite = func(_ context.Context, text string) (clipboardWriteResult, error) {
		f.copied <- text
		return clipboardWriteResult{notice: "Copied for the fixture.", confirmed: true}, nil
	}
	bash := newToolLabel("bash", json.RawMessage(`{"command":"go test ./..."}`), "")
	for _, b := range []Block{
		{Kind: KindUser, Text: menuUserText, Done: true},
		{Kind: KindReasoning, Text: menuReasoningText, Done: true},
		{ID: "ok", Kind: KindTool, Name: "bash", Label: bash, Input: "$ go test ./...", State: StateOK, Detail: "ok  pkg\nPASS\n", Done: true},
		{ID: "running", Kind: KindTool, Name: "bash", Label: bash, Input: "$ sleep 9", State: StateRunning, Lines: []string{"tick 1"}},
		{Kind: KindDiff, Lines: []string{"--- a/x.go", "+++ b/x.go", "+added line"}, Done: true},
		{Kind: KindAssistant, Text: menuReplyText, Done: true},
		{Kind: KindNotice, Text: "a notice line", Done: true},
	} {
		s.Emit(b)
	}
	return f
}

// frame draws the screen at 80 by 50 and returns its text.
func (f *menuFixture) frame() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scr.drain()
	return tui.SprintScreen(f.scr.View(), tui.WithWidth(80), tui.WithHeight(50)).Text()
}

func (f *menuFixture) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// click presses, clicks, and releases button at (x, y), as a terminal and
// Wonton's click detection deliver it.
func (f *menuFixture) click(button tui.MouseButton, x, y int) {
	f.locked(func() {
		for _, typ := range []tui.MouseEventType{tui.MousePress, tui.MouseClick, tui.MouseRelease} {
			f.scr.HandleEvent(tui.MouseEvent{Type: typ, Button: button, X: x, Y: y, ClickCount: 1})
		}
	})
}

// rightClick right-clicks the first cell of text on screen.
func (f *menuFixture) rightClick(text string) {
	f.t.Helper()
	x, y := cellOf(f.t, strings.Split(f.frame(), "\n"), text)
	f.click(tui.MouseButtonRight, x, y)
}

// labels returns the open menu's entries, or nil when it is closed.
func (f *menuFixture) labels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.scr.transcriptMenu.IsOpen() {
		return nil
	}
	var labels []string
	for _, it := range f.scr.transcriptMenu.Items() {
		if !it.Separator {
			labels = append(labels, it.Label)
		}
	}
	return labels
}

// choose selects the entry with label by keyboard and presses Enter.
func (f *menuFixture) choose(label string) {
	f.t.Helper()
	labels := f.labels()
	if !slices.Contains(labels, label) {
		f.t.Fatalf("menu %v has no %q", labels, label)
	}
	f.locked(func() {
		m := &f.scr.transcriptMenu
		for range len(m.Items()) {
			if m.Items()[m.Selected()].Label == label {
				break
			}
			f.scr.HandleEvent(tui.KeyEvent{Key: tui.KeyArrowDown})
		}
		f.scr.HandleEvent(tui.KeyEvent{Key: tui.KeyEnter})
	})
}

func (f *menuFixture) wantCopy(want string) {
	f.t.Helper()
	select {
	case got := <-f.copied:
		if got != want {
			f.t.Fatalf("copied %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		f.t.Fatalf("nothing copied, want %q", want)
	}
	f.waitFor(func() bool {
		for _, b := range f.scr.main().tr.Blocks {
			if b.Kind == KindNotice && b.Text == "Copied for the fixture." {
				return true
			}
		}
		return false
	})
}

func (f *menuFixture) wantNoCopy() {
	f.t.Helper()
	select {
	case got := <-f.copied:
		f.t.Fatalf("copied %q", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRightClickMenuEntriesFitTheItem(t *testing.T) {
	f := newMenuFixture(t)
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"user message", menuUserText, []string{"Copy message"}},
		{"reply", "pass.", []string{"Copy message"}},
		{"reasoning", menuReasoningText, []string{"Copy reasoning"}},
		{"ended tool", "Ran a command", []string{"Expand", "Copy input", "Copy output"}},
		// A running call's output is not complete yet.
		{"running tool", "Running a command", []string{"Expand", "Copy input"}},
		{"diff", "+added line", []string{"Copy diff"}},
		{"notice", "a notice line", nil},
	} {
		f.rightClick(c.text)
		if got := f.labels(); !slices.Equal(got, c.want) {
			t.Errorf("%s: menu %v, want %v", c.name, got, c.want)
		}
		if c.want != nil && !strings.Contains(f.frame(), "│ "+c.want[0]) {
			t.Errorf("%s: menu not drawn:\n%s", c.name, f.frame())
		}
		f.locked(f.scr.transcriptMenu.Close)
	}
}

func TestTranscriptMenuExpandsAndCollapsesAToolRow(t *testing.T) {
	f := newMenuFixture(t)
	f.rightClick("Ran a command")
	f.choose("Expand")
	if got := f.frame(); !strings.Contains(got, "ok  pkg") {
		t.Fatalf("Expand did not show the output:\n%s", got)
	}
	f.rightClick("Ran a command")
	if got := f.labels(); len(got) == 0 || got[0] != "Collapse" {
		t.Fatalf("menu on an expanded row = %v", got)
	}
	f.choose("Collapse")
	if got := f.frame(); strings.Contains(got, "ok  pkg") || f.labels() != nil {
		t.Fatalf("Collapse left the output or the menu:\n%s", got)
	}
}

// Every copy entry copies what a selection of that part copies, through
// the same clipboard path, and reports the result the same way.
func TestTranscriptMenuCopies(t *testing.T) {
	f := newMenuFixture(t)
	for _, c := range []struct{ text, label, want string }{
		{"Ran a command", "Copy input", "$ go test ./..."},
		{"Ran a command", "Copy output", "ok  pkg\nPASS"},
		{menuUserText, "Copy message", menuUserText},
		{"pass.", "Copy message", menuReplyText},
		{menuReasoningText, "Copy reasoning", menuReasoningText},
		{"+added line", "Copy diff", "--- a/x.go\n+++ b/x.go\n+added line"},
	} {
		f.rightClick(c.text)
		f.choose(c.label)
		f.wantCopy(c.want)
	}
	// A click on an entry chooses it too.
	f.rightClick(menuUserText)
	x, y := cellOf(t, strings.Split(f.frame(), "\n"), "Copy message")
	f.click(tui.MouseButtonLeft, x, y)
	f.wantCopy(menuUserText)
}

func TestTranscriptMenuOffersToCopyTheSelection(t *testing.T) {
	f := newMenuFixture(t)
	x, y := cellOf(t, strings.Split(f.frame(), "\n"), menuUserText)
	f.locked(func() {
		f.scr.HandleEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: x, Y: y})
	})
	f.rightClick("Ran a command")
	if got := f.labels(); !slices.Equal(got, []string{"Copy selection", "Expand", "Copy input", "Copy output"}) {
		t.Fatalf("menu with a selection = %v", got)
	}
	if !strings.Contains(f.frame(), "Copy selection   Ctrl+C") {
		t.Fatalf("no Ctrl+C hint:\n%s", f.frame())
	}
	f.choose("Copy selection")
	f.wantCopy(menuUserText)
}

func TestTranscriptMenuClosesWithoutSideEffects(t *testing.T) {
	f := newMenuFixture(t)
	// A selection makes Escape's own effect visible: it would clear it.
	x, y := cellOf(t, strings.Split(f.frame(), "\n"), menuUserText)
	f.locked(func() {
		f.scr.HandleEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: x, Y: y})
	})
	f.rightClick("pass.")
	f.locked(func() { f.scr.HandleEvent(tui.KeyEvent{Key: tui.KeyEscape}) })
	if f.labels() != nil || !f.check(f.scr.main().vp.HasSelection) {
		t.Fatal("Escape did not just close the menu")
	}

	// A click outside on a tool row closes the menu and does not expand it.
	f.rightClick("pass.")
	x, y = cellOf(t, strings.Split(f.frame(), "\n"), "Ran a command")
	f.click(tui.MouseButtonLeft, x, y)
	if f.labels() != nil {
		t.Fatal("a click outside left the menu open")
	}
	if got := f.frame(); strings.Contains(got, "ok  pkg") || !f.check(f.scr.main().vp.HasSelection) {
		t.Fatalf("the click outside reached the transcript:\n%s", got)
	}
	// A right-click outside closes it too, without opening another.
	f.rightClick("pass.")
	x, y = cellOf(t, strings.Split(f.frame(), "\n"), "Ran a command")
	f.click(tui.MouseButtonRight, x, y)
	if f.labels() != nil {
		t.Fatal("a right-click outside left a menu open")
	}
	f.wantNoCopy()
}

func TestRightClickDoesNothingWhileSomethingElseHoldsTheScreen(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *menuFixture)
	}{
		{"approval", func(f *menuFixture) {
			f.scr.HandleEvent(promptEvent{id: 1, approval: &approvalPrompt{reply: make(chan Approval, 1)}})
		}},
		{"question", func(f *menuFixture) {
			f.scr.HandleEvent(promptEvent{id: 1, asking: &askPrompt{reply: make(chan questionAnswer, 1)}})
		}},
		{"picker", func(f *menuFixture) {
			f.scr.HandleEvent(promptEvent{id: 1, picker: &pickerState{title: "Model", options: []string{"a"}, reply: make(chan int, 1)}})
		}},
		{"command menu", func(f *menuFixture) {
			f.scr.input, f.scr.commandMenu.typing = "/co", true
			if f.scr.commandMenuMatches() == nil {
				f.t.Fatal("command menu did not open")
			}
		}},
		{"drag", func(f *menuFixture) {
			x, y := cellOf(f.t, strings.Split(tui.SprintScreen(f.scr.View(), tui.WithWidth(80), tui.WithHeight(50)).Text(), "\n"), menuUserText)
			f.scr.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: tui.MouseButtonLeft, X: x, Y: y})
			f.scr.HandleEvent(tui.MouseEvent{Type: tui.MouseDrag, Button: tui.MouseButtonLeft, X: x + 4, Y: y})
		}},
		{"native selection", func(f *menuFixture) { f.scr.mouseEnabled = false }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMenuFixture(t)
			x, y := cellOf(t, strings.Split(f.frame(), "\n"), "Ran a command")
			f.locked(func() { c.setup(f) })
			f.click(tui.MouseButtonRight, x, y)
			if got := f.labels(); got != nil {
				t.Fatalf("menu opened: %v", got)
			}
			if c.name == "drag" && !f.check(f.scr.main().vp.SelectionActive) {
				t.Fatal("the right-click ended the drag")
			}
		})
	}
}

// The menu stays open while its item streams on, copies the item's text as
// it is when chosen, and closes when the item changes state.
func TestTranscriptMenuFollowsItsItem(t *testing.T) {
	f := newMenuFixture(t)
	f.locked(func() { f.scr.Emit(Block{ID: "live", Kind: KindAssistant, Text: "first part"}) })
	f.rightClick("first part")
	f.locked(func() { f.scr.Emit(Block{ID: "live", Kind: KindAssistant, Text: "first part and more"}) })
	f.frame()
	f.choose("Copy message")
	f.wantCopy("first part and more")

	f.rightClick("Running a command")
	running := Block{ID: "running", Kind: KindTool, Name: "bash", Input: "$ sleep 9", State: StateRunning, Lines: []string{"tick 2"}}
	f.locked(func() { f.scr.Emit(running) })
	f.frame()
	if f.labels() == nil {
		t.Fatal("progress closed the menu")
	}
	running.State, running.Done = StateOK, true
	f.locked(func() { f.scr.Emit(running) })
	f.frame()
	if f.labels() != nil {
		t.Fatal("menu stayed open after its tool call ended")
	}
}

// The wheel does not scroll the transcript under an open menu.
func TestTranscriptMenuKeepsTheWheel(t *testing.T) {
	f := newMenuFixture(t)
	f.locked(func() {
		for range 60 {
			f.scr.Emit(Block{Kind: KindUser, Text: "filler", Done: true})
		}
	})
	x, y := cellOf(t, strings.Split(f.frame(), "\n"), "filler")
	wheel := func() {
		f.locked(func() {
			f.scr.HandleEvent(tui.MouseEvent{Type: tui.MouseScroll, Button: tui.MouseButtonWheelUp, X: x, Y: y})
		})
	}
	f.rightClick("filler")
	wheel()
	if f.labels() == nil || !f.check(func() bool { return f.scr.main().vp.Follow }) {
		t.Fatal("the wheel reached the transcript under the menu")
	}
	// With the menu closed, the same wheel scrolls.
	f.locked(f.scr.transcriptMenu.Close)
	wheel()
	if f.check(func() bool { return f.scr.main().vp.Follow }) {
		t.Fatal("the wheel did not scroll")
	}
}

// A dialog that arrives while the menu is open takes the keys from it.
func TestADialogClosesTheTranscriptMenu(t *testing.T) {
	f := newMenuFixture(t)
	f.rightClick("pass.")
	f.locked(func() {
		f.scr.HandleEvent(promptEvent{id: 1, picker: &pickerState{title: "Model", options: []string{"a"}, reply: make(chan int, 1)}})
	})
	if f.labels() != nil {
		t.Fatal("menu stayed open under a dialog")
	}
}

// Through the real runtime, the focused composer sees no key while the menu
// is open, and Ctrl+C still copies the selection.
func TestRuntimeTranscriptMenuKeepsKeysFromTheComposer(t *testing.T) {
	h := newTranscriptRuntime(t)
	copied := make(chan string, 2)
	h.scr.rt.SendEvent(transcriptCapture{copy: func(_ context.Context, text string) (clipboardWriteResult, error) {
		copied <- text
		return clipboardWriteResult{notice: "Copied in the runtime.", confirmed: true}, nil
	}})
	const reply = "wrapped reply for the menu"
	selectRuntimeLine(t, h, reply)
	h.typeText("draft")
	frame := h.wait(func(s transcriptState) bool { return s.input == "draft" })
	open := func() {
		h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonRight, ClickCount: 1, X: 8, Y: transcriptRow(t, frame.text, "wrapped")})
		h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Copy message") })
	}
	open()
	h.typeText("zq")
	h.key(tui.KeyEvent{Key: tui.KeyArrowDown})
	h.key(tui.KeyEvent{Paste: "pasted"})
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool {
		return !strings.Contains(s.text, "Copy message") && s.input == "draft" && s.selected == reply
	})

	open()
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	select {
	case got := <-copied:
		if got != reply {
			t.Fatalf("Ctrl+C copied %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+C did not copy the selection")
	}
	h.wait(func(s transcriptState) bool {
		return !strings.Contains(s.text, "Copy message") && s.input == "draft" && strings.Contains(s.text, "Copied in the runtime.")
	})
}
