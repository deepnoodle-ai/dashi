package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

// A drag held on the top row keeps scrolling up and growing the selection.
// The transcript starts on the screen's first row, so a terminal can never
// report the pointer above it: the top row has to act as the edge.
func TestDragHeldOnTheTopRowScrollsUpAndExtendsTheSelection(t *testing.T) {
	h := newScreenHarness(t, 0)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range 40 {
		s.Emit(Block{Kind: KindAssistant, Text: fmt.Sprintf("answer %d", i), Done: true})
	}
	s.drain()
	render := func() { tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24)) }
	mouse := func(typ tui.MouseEventType, y int) {
		s.HandleEvent(tui.MouseEvent{Type: typ, Button: tui.MouseButtonLeft, X: 3, Y: y})
	}
	tick := func(n int) {
		for range n {
			s.HandleEvent(tui.TickEvent{})
			render()
		}
	}
	render()
	topBefore, _ := s.vp.Anchor()

	mouse(tui.MousePress, 10)
	mouse(tui.MouseDrag, 0)
	tick(10)

	top, _ := s.vp.Anchor()
	if top >= topBefore {
		t.Fatalf("top item %d, want above %d: the drag did not scroll up", top, topBefore)
	}
	start, _, ok := s.vp.Selection()
	if !ok || start.Item > top+1 {
		t.Fatalf("selection start %+v (ok=%v), want it to follow the scroll to item %d", start, ok, top)
	}
	if got := s.vp.SelectedText(); !strings.Contains(got, fmt.Sprintf("answer %d", top+1)) {
		t.Fatalf("selected %q, want it to reach answer %d", got, top+1)
	}

	// Back inside the viewport, the scrolling stops.
	mouse(tui.MouseDrag, 5)
	stopped, _ := s.vp.Anchor()
	tick(5)
	if now, _ := s.vp.Anchor(); now != stopped {
		t.Fatalf("top item moved from %d to %d after the pointer returned inside", stopped, now)
	}

	// After release, ticks no longer scroll even with the pointer on the top row.
	mouse(tui.MouseDrag, 0)
	mouse(tui.MouseRelease, 0)
	released, _ := s.vp.Anchor()
	tick(5)
	if now, _ := s.vp.Anchor(); now != released {
		t.Fatalf("top item moved from %d to %d after release", released, now)
	}
}

// At the start of the transcript there is nothing to scroll to, so the top
// row selects like any other row.
func TestDragOnTheTopRowAtTheStartSelectsThatRow(t *testing.T) {
	h := newScreenHarness(t, 0)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range 3 {
		s.Emit(Block{Kind: KindAssistant, Text: fmt.Sprintf("answer %d", i), Done: true})
	}
	s.drain()
	screen := tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24)).Text()
	row := -1
	for y, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, "answer 1") {
			row = y
		}
	}
	if row <= 0 {
		t.Fatalf("answer 1 not below the top row:\n%s", screen)
	}
	s.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: tui.MouseButtonLeft, X: 12, Y: row})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseDrag, Button: tui.MouseButtonLeft, X: 3, Y: 0})
	s.HandleEvent(tui.TickEvent{})
	if got, want := s.vp.SelectedText(), "answer 0\nanswer 1"; got != want {
		t.Fatalf("selected %q, want %q", got, want)
	}
}
