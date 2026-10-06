package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/runewidth"
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

// The transcript's last row is not the screen's last row: the composer sits
// below it. A drag held there selects that row without scrolling. Only a drag
// into the rows below scrolls down.
func TestDragOnTheLastTranscriptRowScrollsOnlyBelowIt(t *testing.T) {
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
	s.vp.ScrollBy(-20)
	render()
	last := s.vp.Y + s.vp.Height - 1
	if last >= 23 {
		t.Fatalf("transcript reaches the screen's last row %d; nothing sits below it", last)
	}
	topBefore, _ := s.vp.Anchor()

	mouse(tui.MousePress, 5)
	mouse(tui.MouseDrag, last)
	tick(10)
	if top, _ := s.vp.Anchor(); top != topBefore {
		t.Fatalf("top item moved from %d to %d with the pointer on the last transcript row", topBefore, top)
	}

	mouse(tui.MouseDrag, last+1)
	tick(10)
	if top, _ := s.vp.Anchor(); top <= topBefore {
		t.Fatalf("top item %d, want below %d: a drag under the transcript did not scroll down", top, topBefore)
	}
	mouse(tui.MouseRelease, last+1)
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
	if got, want := s.vp.SelectedText(), "answer 0\n\nanswer 1"; got != want {
		t.Fatalf("selected %q, want %q", got, want)
	}
}

// --- selection across transcript blocks ---

const selW, selH = 60, 100

// selectionTranscript is two turns with the blocks a real session draws
// between them: markdown, a tool line with a task status, a task result,
// and the turn footers.
func selectionTranscript(t *testing.T) *screenHarness {
	t.Helper()
	h := newScreenHarness(t, 0)
	f1 := footerFacts{Turn: 1, Model: "fixture", Calls: 2, Tokens: 1234, Cost: "$0.01"}
	f2 := footerFacts{Turn: 2, Model: "fixture", Calls: 1, Tokens: 99}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range []Block{
		{Kind: KindUser, Text: "Please list the files in this directory and explain how the build works.", Done: true},
		{Kind: KindAssistant, Text: "Here is the plan:\n\n- first item\n- second item\n\n```go\nfunc main() {}\n```\n\nDone with **bold** words.", Done: true},
		{ID: "t1", Kind: KindTool, Name: "bash", Arg: "ls -la", State: StateOK, Elapsed: 1200 * time.Millisecond, Lines: []string{"file one", "file two"}, Done: true},
		{Kind: KindTaskStatus, Parent: "t1", Name: "task-1", Text: "running", State: StateOK, Done: true},
		{Kind: KindFooter, Text: f1.String(), Footer: &f1, Done: true},
		{Kind: KindUser, Text: "Second question", Done: true},
		{Kind: KindAssistant, Text: "Second answer.", Done: true},
		{Kind: KindTaskResult, Arg: "task-1", Name: "done", Text: "result here", Done: true},
		{Kind: KindFooter, Text: f2.String(), Footer: &f2, Done: true},
	} {
		h.scr.Emit(b)
	}
	h.scr.drain()
	return h
}

// selectionFrame renders the screen and returns each row's text and the
// text of its highlighted cells.
func selectionFrame(s *screen) (rows, lit []string) {
	frame := tui.SprintScreen(s.View(), tui.WithWidth(selW), tui.WithHeight(selH))
	for y := range selH {
		var row, hl strings.Builder
		for x := range selW {
			g := frame.CellGlyph(x, y)
			if g == "" {
				g = " "
			}
			row.WriteString(g)
			if frame.Cell(x, y).Style.Reverse {
				hl.WriteString(g)
			}
		}
		rows = append(rows, row.String())
		lit = append(lit, hl.String())
	}
	return rows, lit
}

// cellOf is the screen cell where text first appears.
func cellOf(t *testing.T, rows []string, text string) (x, y int) {
	t.Helper()
	for y, row := range rows {
		if i := strings.Index(row, text); i >= 0 {
			return runewidth.StringWidth(row[:i]), y
		}
	}
	t.Fatalf("missing %q in\n%s", text, strings.Join(rows, "\n"))
	return 0, 0
}

// drag presses at one cell, drags to the other, and releases there.
func drag(s *screen, x0, y0, x1, y1 int) {
	s.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: tui.MouseButtonLeft, X: x0, Y: y0})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseDrag, Button: tui.MouseButtonLeft, X: x1, Y: y1})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseRelease, Button: tui.MouseButtonLeft, X: x1, Y: y1})
}

// A drag across a turn boundary copies the text it crosses, the tool's name
// and argument included, and leaves out the glyphs, the tool's state, and the
// footer's rule. Forward and backward drags copy the same text.
func TestDragAcrossATurnCopiesTextWithoutDecorations(t *testing.T) {
	h := selectionTranscript(t)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, _ := selectionFrame(s)
	fx, fy := cellOf(t, rows, "Done with")
	tx, ty := cellOf(t, rows, "question")

	drag(s, fx, fy, tx, ty)
	forward := s.vp.SelectedText()
	_, lit := selectionFrame(s)
	drag(s, tx, ty, fx, fy)
	if backward := s.vp.SelectedText(); backward != forward {
		t.Fatalf("backward copy %q, forward %q", backward, forward)
	}
	for _, want := range []string{"Done with **bold** words.", "bash  ls -la", "file one", "file two", "turn 1 · fixture · 2 model calls", "Second "} {
		if !strings.Contains(forward, want) {
			t.Errorf("copy %q lacks %q", forward, want)
		}
	}
	for _, decoration := range []string{"──", "⏺", "❯", "✓", "ok  1.2s", "question"} {
		if strings.Contains(forward, decoration) {
			t.Errorf("copy %q includes %q", forward, decoration)
		}
	}
	_, toolRow := cellOf(t, rows, "bash")
	_, footerRow := cellOf(t, rows, "turn 1")
	if !strings.Contains(lit[toolRow], "bash") || strings.Contains(lit[toolRow], "ok") {
		t.Errorf("tool row highlight %q, want the name and not the state", lit[toolRow])
	}
	if !strings.Contains(lit[footerRow], "turn") || strings.Contains(lit[footerRow], "─") {
		t.Errorf("footer highlight %q, want the values and not the rule", lit[footerRow])
	}
}

// A task result line used to have no selectable text, so Wonton could not
// map it, and a drag that reached it dropped the whole selection.
func TestDragThroughATaskResultKeepsTheSelection(t *testing.T) {
	h := selectionTranscript(t)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, _ := selectionFrame(s)
	fx, fy := cellOf(t, rows, "Second answer")
	tx, ty := cellOf(t, rows, "turn 2")
	_, ry := cellOf(t, rows, "result here")

	for _, ends := range [][4]int{{fx, fy, tx + 4, ty}, {tx + 4, ty, fx, fy}} {
		drag(s, ends[0], ends[1], ends[2], ends[3])
		got := s.vp.SelectedText()
		if !strings.Contains(got, "Second answer.") || !strings.Contains(got, "result here") || strings.Contains(got, "↳") {
			t.Fatalf("drag %v copied %q", ends, got)
		}
		if _, lit := selectionFrame(s); !strings.Contains(lit[ry], "result") {
			t.Fatalf("task result row highlight %q", lit[ry])
		}
	}
}

// Every kind of block can sit in the middle of a selection, including a
// blank reply, which draws nothing and copies nothing.
func TestADragSurvivesEveryKindOfBlock(t *testing.T) {
	f := footerFacts{Turn: 1, Model: "fixture", Calls: 1, Tokens: 10}
	blocks := []Block{
		{Kind: KindUser, Text: "user text"},
		{Kind: KindAssistant, Text: "assistant *text*"},
		{Kind: KindAssistant, Text: "  "},
		{Kind: KindReasoning, Text: "reasoning text"},
		{Kind: KindReasoning, Text: " "},
		{Kind: KindTool, Name: "bash", Arg: "go test", State: StateRunning},
		{Kind: KindTool, Name: "read_file", Arg: "a.go", State: StateFailed, Lines: []string{"no such file"}},
		{Kind: KindTool, Name: "grep", State: StateOK, Lines: []string{"preview"}, Detail: "preview\nmore"},
		{Kind: KindTool, Name: "task", Arg: "x", State: StateOK, Task: &Block{Kind: KindTaskStatus, Name: "task-1", Text: "running", State: StateRunning}},
		{Kind: KindTaskStatus, Name: "task-2", Text: "orphan status", State: StateOK},
		{Kind: KindTaskResult, Arg: "task-1", Name: "done", Text: "result"},
		{Kind: KindDiff, Lines: []string{"--- a", "+++ b", "@@ -1 +1 @@", "-old", "+new"}},
		{Kind: KindSteer, Text: "steer text", State: SteerQueued},
		{Kind: KindStop, Text: "stopped"},
		{Kind: KindFooter, Text: f.String(), Footer: &f},
		{Kind: KindFooter, Text: "── plain footer"},
		{Kind: KindNotice, Text: "notice text"},
		{Kind: KindWarning, Text: "warning text"},
		{Kind: KindCompaction, Text: "compacted"},
		{Kind: KindPending, Text: "pending text"},
		{Kind: KindPending, Text: "sent", Done: true},
		{Kind: KindHelp},
	}
	for _, b := range blocks {
		t.Run(string(b.Kind)+" "+b.Text, func(t *testing.T) {
			h := newScreenHarness(t, 0)
			s := h.scr
			h.mu.Lock()
			defer h.mu.Unlock()
			if b.Kind == KindHelp {
				b = s.ctrl.helpBlock()
			}
			b.Done = true
			for _, b := range []Block{{Kind: KindNotice, Text: "BEFORE"}, b, {Kind: KindNotice, Text: "AFTER"}} {
				s.Emit(b)
			}
			s.drain()
			s.vp.ScrollToTop()
			rows, _ := selectionFrame(s)
			bx, by := cellOf(t, rows, "BEFORE")
			ax, ay := cellOf(t, rows, "AFTER")
			drag(s, bx, by, ax+5, ay)
			if got := s.vp.SelectedText(); !strings.HasPrefix(got, "BEFORE") || !strings.HasSuffix(got, "AFTER") {
				t.Fatalf("selection across %s block copied %q", b.Kind, got)
			}
		})
	}
}

// A drag held on the top row scrolls up through several turns. The end the
// user pressed stays put, and the copy grows across every kind of block.
func TestDragAutoScrollAcrossTurnsKeepsTheAnchor(t *testing.T) {
	h := newScreenHarness(t, 0)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	for turn := 1; turn <= 4; turn++ {
		f := footerFacts{Turn: turn, Model: "fixture", Calls: 1, Tokens: 10}
		id := fmt.Sprintf("t%d", turn)
		for _, b := range []Block{
			{Kind: KindUser, Text: fmt.Sprintf("question %d", turn)},
			{Kind: KindAssistant, Text: fmt.Sprintf("Answer %d:\n\n- a list item\n\n```sh\nmake test\n```", turn)},
			{ID: id, Kind: KindTool, Name: "bash", Arg: "make test", State: StateOK, Lines: []string{"ok"}},
			{Kind: KindTaskStatus, Parent: id, Name: "task", Text: "running", State: StateRunning},
			{Kind: KindTaskResult, Arg: "task", Name: "done", Text: fmt.Sprintf("result %d", turn)},
			{Kind: KindFooter, Text: f.String(), Footer: &f},
		} {
			b.Done = true
			s.Emit(b)
		}
	}
	s.drain()
	render := func() string { return tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24)).Text() }
	rows := strings.Split(render(), "\n")
	x, y := cellOf(t, rows, "result 4")
	x += len("result 4")

	s.HandleEvent(tui.MouseEvent{Type: tui.MousePress, Button: tui.MouseButtonLeft, X: x, Y: y})
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseDrag, Button: tui.MouseButtonLeft, X: 3, Y: 0})
	for range 80 {
		s.HandleEvent(tui.TickEvent{})
		render()
	}
	s.HandleEvent(tui.MouseEvent{Type: tui.MouseRelease, Button: tui.MouseButtonLeft, X: 3, Y: 0})
	got := s.vp.SelectedText()
	if !strings.HasSuffix(got, "result 4") {
		t.Fatalf("the pressed end moved: copy ends %q", got[max(0, len(got)-40):])
	}
	for _, want := range []string{"Answer 1:", "bash  make test", "result 1", "turn 1 · fixture", "question 2", "turn 3 · fixture"} {
		if !strings.Contains(got, want) {
			t.Errorf("copy lacks %q:\n%s", want, got)
		}
	}
	for _, decoration := range []string{"──", "⏺", "❯", "↳", "⎿"} {
		if strings.Contains(got, decoration) {
			t.Errorf("copy includes %q:\n%s", decoration, got)
		}
	}
}

// What the highlight shows is exactly what /copy copies: blank lines between
// blocks as on screen, one continuous band per row, and decoration left out
// of both even when the drag starts on it.
func TestDragAcrossATurnMatchesTheHighlight(t *testing.T) {
	h := selectionTranscript(t)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, _ := selectionFrame(s)
	row := func(text string) int { _, y := cellOf(t, rows, text); return y }

	fx, fy := cellOf(t, rows, "Done with")
	tx, ty := cellOf(t, rows, "question")
	drag(s, fx, fy, tx, ty)
	want := "Done with **bold** words.\n\nbash  ls -la\nfile one\nfile two\n\nturn 1 · fixture · 2 model calls · 1,234 tokens · $0.01\n\nSecond "
	if got := s.vp.SelectedText(); got != want {
		t.Fatalf("copy %q\nwant %q", got, want)
	}
	_, lit := selectionFrame(s)
	for text, band := range map[string]string{
		"Done with":    "Done with bold words.",
		"bash":         "bash  ls -la",
		"file one":     "file one",
		"turn 1":       "turn 1 · fixture · 2 model calls · 1,234 tokens · $0.01",
		"Second quest": "Second ",
		"⎿":            "",
	} {
		if got := lit[row(text)]; got != band {
			t.Errorf("row %q highlights %q, want %q", text, got, band)
		}
	}
	if got := lit[fy+1] + lit[ty-1]; got != "" {
		t.Errorf("gap rows highlight %q", got)
	}

	// The drag may start on decoration: the • glyph, a list bullet, the
	// footer rule, or the gap row above a block.
	gx, gy := cellOf(t, rows, "• Second")
	ex, _ := cellOf(t, rows, "answer.")
	bx, by := cellOf(t, rows, "• second")
	ix, _ := cellOf(t, rows, "second item")
	rx, ry := cellOf(t, rows, "── turn 1")
	cx, cy := cellOf(t, rows, "$0.01")
	for _, c := range []struct {
		name           string
		x0, y0, x1, y1 int
		want           string
	}{
		{"glyph forward", gx, gy, ex + 7, gy, "Second answer."},
		{"glyph backward", ex + 7, gy, gx, gy, "Second answer."},
		{"bullet forward", bx, by, ix + 11, by, "second item"},
		{"rule forward", rx, ry, rx + 9, ry, "turn 1"},
		{"gap row forward", 3, ty - 1, tx, ty, "Second "},
		{"gap row backward", 3, ty - 1, cx, cy, "$0.01"},
	} {
		drag(s, c.x0, c.y0, c.x1, c.y1)
		if got := s.vp.SelectedText(); got != c.want {
			t.Errorf("%s: copy %q, want %q", c.name, got, c.want)
		}
	}
}

// Drive the focused input, rather than calling screen.key directly: Wonton
// forwards Ctrl-C even when the input consumes a key.
func TestRuntimeSelectedCopyPreservesDraftAndReportsReceipt(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unconfirmed", "failed", "osc52"} {
		t.Run(outcome, func(t *testing.T) {
			h := newTranscriptRuntime(t)
			copied := make(chan string, 2)
			var output io.Writer = failedClipboardOutput{}
			if outcome == "osc52" {
				output = io.Discard
			}
			h.scr.rt.SendEvent(transcriptCapture{out: output, copy: func(_ context.Context, text string) (clipboardWriteResult, error) {
				copied <- text
				if outcome == "failed" || outcome == "osc52" {
					return clipboardWriteResult{}, errors.New("fixture unavailable")
				}
				return clipboardWriteResult{notice: outcome + " clipboard fixture", confirmed: outcome == "confirmed"}, nil
			}})
			source := "**wrapped**\t世界 " + strings.Repeat("long ", 24) + "END  "
			selectRuntimeLine(t, h, source)
			h.typeText("keep this draft")
			h.wait(func(s transcriptState) bool { return s.input == "keep this draft" && s.selected == source })
			h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
			select {
			case got := <-copied:
				if got != source {
					t.Fatalf("copied %q, want exact unwrapped source %q", got, source)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("copy did not reach writer")
			}
			message := outcome + " clipboard fixture"
			if outcome == "failed" {
				message = "Clipboard request failed"
			} else if outcome == "osc52" {
				message = "Sent a terminal clipboard request"
			}
			h.wait(func(s transcriptState) bool {
				selected := source
				if outcome == "confirmed" {
					selected = ""
				}
				return s.selected == selected && s.input == "keep this draft" && strings.Contains(s.text, message)
			})
			select {
			case <-copied:
				t.Fatal("one Ctrl-C copied twice")
			default:
			}
		})
	}
}

type failedClipboardOutput struct{}

func (failedClipboardOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func selectRuntimeLine(t *testing.T, h *runtimeFixture, source string) {
	t.Helper()
	h.scr.Emit(Block{Kind: KindAssistant, Text: source, Done: true})
	frame := h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "wrapped") })
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: 8, Y: transcriptRow(t, frame.text, "wrapped")})
	h.wait(func(s transcriptState) bool { return s.selected == source })
}

func TestRuntimeCopyCompletionCannotClearNewSelection(t *testing.T) {
	for _, change := range []string{"reselect same text", "new session"} {
		t.Run(change, func(t *testing.T) {
			h := newTranscriptRuntime(t)
			started, release := make(chan struct{}), make(chan struct{})
			h.scr.rt.SendEvent(transcriptCapture{copy: func(ctx context.Context, text string) (clipboardWriteResult, error) {
				close(started)
				select {
				case <-release:
					return clipboardWriteResult{notice: "Old copy finished", confirmed: true}, nil
				case <-ctx.Done():
					return clipboardWriteResult{}, ctx.Err()
				}
			}})
			const source = "wrapped selection stays"
			selectRuntimeLine(t, h, source)
			h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
			<-started
			if change == "new session" {
				h.app.ctrl.Input("/new")
				h.app.ctrl.Wait()
				h.wait(func(s transcriptState) bool { return s.selected == "" })
				selectRuntimeLine(t, h, source)
			} else {
				frame := h.wait(func(s transcriptState) bool { return s.selected == source })
				h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: 8, Y: transcriptRow(t, frame.text, "wrapped")})
			}
			// A probe ensures that re-selection has run before completion.
			h.wait(func(s transcriptState) bool { return s.selected == source })
			close(release)
			h.wait(func(s transcriptState) bool {
				return s.selected == source && strings.Contains(s.text, "Old copy finished")
			})
		})
	}
}

func TestRuntimeSelectionCopyDuringWorkKeepsEscapeInterruption(t *testing.T) {
	started := make(chan struct{})
	h := newTranscriptRuntime(t, blockUntilDone(started))
	h.scr.rt.SendEvent(transcriptCapture{copy: func(context.Context, string) (clipboardWriteResult, error) {
		return clipboardWriteResult{notice: "Copied while running", confirmed: true}, nil
	}})
	selectRuntimeLine(t, h, "wrapped previous reply")
	h.typeText("start work")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	<-started
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeRunning && s.selected == "" && strings.Contains(s.text, "Copied while running")
	})
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && strings.Contains(s.text, "Interrupted") })
}

func TestRuntimeF2PreservesDraftAndClearsApplicationSelection(t *testing.T) {
	h := newTranscriptRuntime(t)
	selectRuntimeLine(t, h, "wrapped previous reply")
	h.key(tui.KeyEvent{Paste: "draft 世界\nsecond"})
	h.wait(func(s transcriptState) bool { return s.input == "draft 世界\nsecond" })
	h.key(tui.KeyEvent{Key: tui.KeyF2})
	h.wait(func(s transcriptState) bool {
		return !s.mouse && s.selected == "" && s.input == "draft 世界\nsecond" && strings.Contains(s.text, "capture off")
	})
	h.key(tui.KeyEvent{Key: tui.KeyF2})
	h.wait(func(s transcriptState) bool {
		return s.mouse && s.input == "draft 世界\nsecond" && strings.Contains(s.text, "capture on")
	})
}

func TestComposerShortcutsYieldToDialogs(t *testing.T) {
	for _, dialog := range []string{"approval", "denial", "question", "own answer", "picker", "status"} {
		t.Run(dialog, func(t *testing.T) {
			h := selectionTranscript(t)
			h.mu.Lock()
			defer h.mu.Unlock()
			s := h.scr
			s.clipboardWrite = func(context.Context, string) (clipboardWriteResult, error) {
				return clipboardWriteResult{}, errors.New("dialog must not copy")
			}
			rows, _ := selectionFrame(s)
			x, y := cellOf(t, rows, "Second answer.")
			drag(s, x, y, x+6, y)
			s.runtime = tui.NewRuntime(tui.NewTestTerminal(80, 24, io.Discard), s, 30)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.ctrl.mu.Lock()
			s.ctrl.cancel, s.ctrl.mode = cancel, ModeRunning
			s.ctrl.mu.Unlock()
			s.promptActive = true
			pickerReply := make(chan int, 1)
			switch dialog {
			case "approval", "denial":
				s.approval = &approvalPrompt{denying: dialog == "denial"}
			case "question", "own answer":
				s.asking = &askPrompt{own: dialog == "own answer"}
			case "picker":
				s.picker = &pickerState{reply: pickerReply}
			case "status":
				s.dialogStatus = &dialogStatusInput{}
			}
			if s.selectionHint() != "" {
				t.Fatal("dialog advertises main-composer shortcut")
			}
			s.key(tui.KeyEvent{Key: tui.KeyF2})
			if !s.mouseEnabled {
				t.Fatal("F2 toggled capture behind dialog")
			}
			s.key(tui.KeyEvent{Key: tui.KeyCtrlC})
			if dialog == "picker" {
				select {
				case got := <-pickerReply:
					if got != -1 {
						t.Fatal("picker cancellation selected an entry")
					}
				default:
					t.Fatal("Ctrl-C copied instead of canceling picker")
				}
			} else if ctx.Err() == nil {
				t.Fatal("Ctrl-C copied instead of canceling dialog work")
			}
			// Undo synthetic state before harness teardown.
			s.approval, s.asking, s.picker, s.dialogStatus = nil, nil, nil, nil
			s.promptActive = false
			s.ctrl.setMode(ModeIdle)
		})
	}
}

func TestInlineF2KeepsDraftAndNativeSelection(t *testing.T) {
	h := newScreenHarness(t, 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.scr
	send := s.send
	newInlineScreen(s, io.Discard)
	s.send = send
	s.input = "draft 世界\nsecond"
	s.key(tui.KeyEvent{Key: tui.KeyF2})
	s.drain()
	if s.mouseEnabled || s.input != "draft 世界\nsecond" || !strings.Contains(s.tr.Blocks[len(s.tr.Blocks)-1].Text, "Native selection is already available") {
		t.Fatal("inline F2 changed the draft or native selection mode")
	}
}
