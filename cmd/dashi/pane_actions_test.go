package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

// panesOn is a 120×40 screen with a task pane and the details pane open,
// drawn once so positions are current. The task pane holds enough text to
// scroll.
func panesOn(t *testing.T) (*screen, []string) {
	t.Helper()
	s := sidePaneFixture(t, 120, 40, true)
	task := s.panes.get("task:t_k3f2").view
	for i := range 40 {
		task.apply(Block{ID: fmt.Sprintf("line%d", i), Kind: KindAssistant, Text: fmt.Sprintf("task line %d", i), Done: true})
	}
	return s, strings.Split(frameText(s), "\n")
}

func click(s *screen, button tui.MouseButton, x, y int) {
	s.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: button, X: x, Y: y})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseRelease, Button: button, X: x, Y: y})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseClick, Button: button, ClickCount: 1, X: x, Y: y})
}

func altKey(key tui.Key, r rune) tui.KeyEvent { return tui.KeyEvent{Key: key, Rune: r, Alt: true} }

func TestPaneClicksActivateAndTitleMenu(t *testing.T) {
	s, rows := panesOn(t)
	if s.panes.active != detailsPaneID {
		t.Fatalf("the last opened pane is not active: %s", s.panes.active)
	}
	x, y := cellOf(t, rows, "I’ll inspect")
	click(s, tui.MouseButtonLeft, x, y)
	if s.panes.active != mainPaneID {
		t.Fatalf("a click in main did not activate it: %s", s.panes.active)
	}
	rows = strings.Split(frameText(s), "\n")
	x, y = cellOf(t, rows, "t_k3f2 · explore")
	click(s, tui.MouseButtonRight, x, y)
	if s.panes.active != "task:t_k3f2" || !s.transcriptMenu.IsOpen() {
		t.Fatal("a right click on a title did not activate and open the menu")
	}
	var labels []string
	for _, item := range s.transcriptMenu.Items() {
		labels = append(labels, item.Label)
	}
	if strings.Join(labels, ",") != "Zoom,Equalize,Close" {
		t.Fatalf("menu=%v", labels)
	}
	s.transcriptMenu.Choose(0)
	if s.panes.zoom != "task:t_k3f2" {
		t.Fatal("Zoom did nothing")
	}
	s.panes.zoom = ""
	rows = strings.Split(frameText(s), "\n")
	x, y = cellOf(t, rows, "main")
	click(s, tui.MouseButtonRight, x, y)
	if items := s.transcriptMenu.Items(); !items[2].Disabled {
		t.Fatal("Close is offered on main")
	}
}

func TestWheelScrollsThePaneUnderThePointer(t *testing.T) {
	s, rows := panesOn(t)
	task := s.panes.get("task:t_k3f2").view
	x, y := cellOf(t, rows, "task line 39")
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseScroll, Button: tui.MouseButtonWheelUp, X: x, Y: y})
	frameText(s)
	if task.vp.AtBottom || s.main().vp.Follow == false {
		t.Fatal("the wheel did not scroll only the task pane")
	}
	if s.panes.active != detailsPaneID {
		t.Fatal("the wheel activated a pane")
	}
	// PgUp and the lines-below line act on the active pane.
	s.activatePane("task:t_k3f2")
	if got := frameText(s); !strings.Contains(got, "lines below") {
		t.Fatalf("no lines-below line for the active pane:\n%s", got)
	}
	s.HandleEvent(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	frameText(s)
	if !task.vp.AtBottom {
		t.Fatal("Ctrl+End did not return the active pane to live output")
	}
}

func TestOneSelectionAcrossPanes(t *testing.T) {
	s, rows := panesOn(t)
	var copied string
	done := make(chan struct{})
	s.clipboardWrite = func(_ context.Context, text string) (clipboardWriteResult, error) {
		copied = text
		close(done)
		return clipboardWriteResult{notice: "Copied.", confirmed: true}, nil
	}
	s.send = func(tui.Event) {}
	mx, my := cellOf(t, rows, "I’ll inspect")
	drag(s, mx, my, mx+10, my)
	if !s.main().vp.HasSelection() {
		t.Fatal("no selection in main")
	}
	tx, ty := cellOf(t, rows, "task line 39")
	drag(s, tx, ty, tx+8, ty)
	task := s.panes.get("task:t_k3f2").view
	if s.main().vp.HasSelection() || !task.vp.HasSelection() {
		t.Fatal("a selection in the task pane must clear main's")
	}
	s.HandleEvent(tui.KeyEvent{Key: tui.KeyCtrlC})
	<-done
	if !strings.HasPrefix(copied, "task lin") {
		t.Fatalf("Ctrl+C copied %q, want the task pane's selection", copied)
	}
	// Esc clears the selection wherever it is, then scrolls the active
	// pane to the bottom.
	s.HandleEvent(tui.KeyEvent{Key: tui.KeyEscape})
	if task.vp.HasSelection() {
		t.Fatal("Esc did not clear the task pane's selection")
	}
}

func TestAltKeysMoveZoomAndClose(t *testing.T) {
	s, _ := panesOn(t)
	s.activatePane(mainPaneID)
	frameText(s)
	s.HandleEvent(altKey(tui.KeyArrowRight, 0))
	if s.panes.active != "task:t_k3f2" {
		t.Fatalf("Alt+Right from main went to %s", s.panes.active)
	}
	frameText(s)
	s.HandleEvent(altKey(tui.KeyArrowDown, 0))
	if s.panes.active != detailsPaneID {
		t.Fatalf("Alt+Down went to %s", s.panes.active)
	}
	s.HandleEvent(altKey(tui.KeyUnknown, 'z'))
	if s.panes.zoom != detailsPaneID {
		t.Fatal("Alt+Z did not zoom")
	}
	frameText(s)
	// An arrow ends zoom and, with stale positions, steps in layout order.
	s.HandleEvent(altKey(tui.KeyArrowLeft, 0))
	if s.panes.zoom != "" || s.panes.active != "task:t_k3f2" {
		t.Fatalf("zoom=%q active=%q", s.panes.zoom, s.panes.active)
	}
	s.HandleEvent(altKey(tui.KeyUnknown, 'W'))
	if s.panes.get("task:t_k3f2") != nil || s.panes.active != mainPaneID {
		t.Fatal("Alt+W did not close the active pane")
	}
	s.HandleEvent(altKey(tui.KeyUnknown, 'w'))
	if s.panes.get(mainPaneID) == nil || s.panes.count() != 2 {
		t.Fatal("Alt+W on main closed something")
	}
}

func TestAltKeysInComposerAndDialogs(t *testing.T) {
	s, _ := panesOn(t)
	var posted []tui.Event
	s.send = func(e tui.Event) { posted = append(posted, e) }
	s.input = "draft"
	if !s.inputKey(altKey(tui.KeyUnknown, 'z')) || len(posted) != 1 {
		t.Fatal("the composer did not take Alt+Z for the loop")
	}
	if s.input != "draft" {
		t.Fatal("Alt+Z changed the draft")
	}
	posted = nil
	s.promptActive = true
	s.asking = &askPrompt{q: Question{Prompt: "Why?"}, own: true, selected: map[int]bool{}}
	if !s.dialogTextKey(s.promptID)(altKey(tui.KeyArrowLeft, 0)) || len(posted) != 0 {
		t.Fatal("a dialog text field must swallow pane keys and do nothing")
	}
}

func TestPaneCommand(t *testing.T) {
	s, _ := panesOn(t)
	run := func(arg string) string {
		s.transcriptCommand("/pane", arg)
		s.drain()
		b := s.main().tr.Blocks
		return b[len(b)-1].Text
	}
	run("focus t_k3f2")
	if s.panes.active != "task:t_k3f2" {
		t.Fatal("/pane focus ID")
	}
	run("focus main")
	run("next")
	if s.panes.active != "task:t_k3f2" {
		t.Fatalf("/pane next went to %s", s.panes.active)
	}
	run("zoom")
	if s.panes.zoom != "task:t_k3f2" {
		t.Fatal("/pane zoom")
	}
	run("focus details")
	if s.panes.zoom != "" || s.panes.active != detailsPaneID {
		t.Fatal("/pane focus must end another pane's zoom")
	}
	if got := run("focus nope"); !strings.Contains(got, "Open panes: main, t_k3f2, details") {
		t.Fatalf("unknown pane reply %q", got)
	}
	if got := run("sideways"); !strings.Contains(got, "Usage: /pane") {
		t.Fatalf("bad arg reply %q", got)
	}
	run("equalize")
	run("close")
	if s.panes.get(detailsPaneID) != nil || s.panes.active != mainPaneID {
		t.Fatal("/pane close")
	}
	if got := run("close"); !strings.Contains(got, "cannot be closed") {
		t.Fatalf("closing main replied %q", got)
	}
}

func TestPaneCommandNeedsFullscreen(t *testing.T) {
	h := newHarness(t)
	c := h.app.ctrl
	var out bytes.Buffer
	c.ui = newLineUI(&out)
	c.command("/pane zoom")
	if !strings.Contains(out.String(), "Panes need fullscreen mode.") {
		t.Fatalf("plain mode replied %q", out.String())
	}
	inline := newInlineScreen(newScreen("fixture"), &out)
	c.ui = inline
	c.command("/pane zoom")
	inline.screen.drain()
	b := inline.main().tr.Blocks
	if len(b) == 0 || !strings.Contains(b[len(b)-1].Text, "Panes need fullscreen mode.") {
		t.Fatalf("inline mode replied %v", b)
	}
}

func TestResizeAcrossTheFoldKeepsState(t *testing.T) {
	s, _ := panesOn(t)
	s.input = "half-typed"
	task := s.panes.get("task:t_k3f2").view
	for i := 40; i < 200; i++ {
		task.apply(Block{ID: fmt.Sprintf("line%d", i), Kind: KindAssistant, Text: fmt.Sprintf("task line %d", i), Done: true})
	}
	s.activatePane("task:t_k3f2")
	frameText(s)
	task.vp.ScrollBy(-120)
	frameText(s)
	// topItem is the first item drawn in the task pane.
	topItem := func() int {
		for y := range 3 {
			if i, _, ok := task.vp.ItemAt(0, y); ok {
				return i
			}
		}
		return -1
	}
	top := topItem()
	for _, w := range []int{80, 119, 120, 200, 40} {
		s.HandleEvent(tui.ResizeEvent{Width: w, Height: 30})
		frameText(s)
		if s.input != "half-typed" || s.panes.active != "task:t_k3f2" || s.panes.count() != 3 || task.vp.Follow {
			t.Fatalf("width %d lost state: input=%q active=%s count=%d follow=%v", w, s.input, s.panes.active, s.panes.count(), task.vp.Follow)
		}
		if i := topItem(); i != top {
			t.Fatalf("width %d moved the task pane from item %d to %d", w, top, i)
		}
	}
	// Narrow mode: Alt+Up and Alt+Down step through tabs like Left and
	// Right; a click on a tab activates its pane.
	s.HandleEvent(altKey(tui.KeyArrowDown, 0))
	if s.panes.active != detailsPaneID {
		t.Fatalf("narrow Alt+Down went to %s", s.panes.active)
	}
	s.HandleEvent(altKey(tui.KeyArrowUp, 0))
	if s.panes.active != "task:t_k3f2" {
		t.Fatalf("narrow Alt+Up went to %s", s.panes.active)
	}
	rows := strings.Split(frameText(s), "\n")
	x, y := cellOf(t, rows, "main")
	click(s, tui.MouseButtonLeft, x, y)
	if s.panes.active != mainPaneID {
		t.Fatal("a click on a tab did not activate its pane")
	}
}

func TestResetClosesSidePanes(t *testing.T) {
	s, _ := panesOn(t)
	s.Reset()
	s.drain()
	if s.panes.count() != 1 || s.panes.active != mainPaneID || s.panes.hinted {
		t.Fatal("Reset left side panes open")
	}
}

func TestGutterDragResizes(t *testing.T) {
	s, rows := panesOn(t)
	x, _ := cellOf(t, rows, "│")
	before := s.panes.split.Root.Children[0].Weight
	s.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: tui.MouseButtonLeft, X: x, Y: 5})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseDrag, Button: tui.MouseButtonLeft, X: x - 20, Y: 5})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseRelease, Button: tui.MouseButtonLeft, X: x - 20, Y: 5})
	if s.panes.split.Root.Children[0].Weight >= before {
		t.Fatal("dragging the gutter left did not shrink main")
	}
	if s.main().vp.HasSelection() {
		t.Fatal("a gutter drag selected text")
	}
}
