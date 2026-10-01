package main

import (
	"fmt"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func TestMouseWheelScrollsThreeLines(t *testing.T) {
	h := newScreenHarness(t, 0)
	for i := range 60 {
		h.scr.Emit(Block{Kind: KindNotice, Text: fmt.Sprintf("line %d", i), Done: true})
	}
	h.render()
	wheel := func(button tui.MouseButton) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.scr.HandleEvent(tui.MouseEvent{Type: tui.MouseScroll, Button: button, X: 2, Y: 2})
	}
	below := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.scr.vp.LinesBelow
	}
	wheel(tui.MouseButtonWheelUp)
	first := below()
	wheel(tui.MouseButtonWheelUp)
	if first == 0 || below()-first != wheelStep {
		t.Fatalf("below after two notches = %d, %d", first, below())
	}
	wheel(tui.MouseButtonWheelDown)
	if below() != first {
		t.Fatalf("below after scrolling back = %d, want %d", below(), first)
	}
}

func TestMouseWheelScrollsOpenDialog(t *testing.T) {
	s := newScreen("fixture")
	lines := make(detailLines, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("detail %d", i)
	}
	p := &pickerState{title: "Task 1", options: []string{"Close"}, details: []string{"x"}, sessionCards: true, reply: make(chan int, 1)}
	s.HandleEvent(promptEvent{id: 1, picker: p})
	tui.SprintScreen(tui.Height(6, tui.Viewport(&p.sessionVP, lines).Gap(0)), tui.WithWidth(40), tui.WithHeight(6))
	before := p.sessionVP.LinesBelow
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseScroll, Button: tui.MouseButtonWheelDown, X: 2, Y: 2})
	if before == 0 || before-p.sessionVP.LinesBelow != wheelStep {
		t.Fatalf("dialog lines below = %d, then %d", before, p.sessionVP.LinesBelow)
	}
}
