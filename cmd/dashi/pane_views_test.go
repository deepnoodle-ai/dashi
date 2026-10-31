package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

// openTestPanes opens a task pane and, with details, the details pane, each
// holding static text, as later phases will fill them.
func openTestPanes(s *screen, details bool) {
	task := s.newPane("task:t_k3f2", paneTask, "t_k3f2")
	task.title = "explore · running · 3 model calls · $0.0123"
	task.view.apply(Block{ID: "a", Kind: KindAssistant, Text: "Reading the composer code for the task.", Done: true})
	s.openPane(task)
	if details {
		d := s.newPane(detailsPaneID, paneDetails, "details")
		d.title = "/tasks"
		d.view.apply(Block{Kind: KindNotice, Text: "t_k3f2 explore running", Done: true})
		s.openPane(d)
	}
	s.drain()
}

// sidePaneFixture is the visual fixture with side panes open.
func sidePaneFixture(t *testing.T, width, height int, details bool) *screen {
	t.Helper()
	s := terminalVisualFixture(t, "success", width, height)
	openTestPanes(s, details)
	return s
}

// frameText renders the screen twice, so positions from the first frame
// are current, and returns the second frame's text.
func frameText(s *screen) string {
	tui.SprintScreen(s.View(), tui.WithWidth(s.width), tui.WithHeight(s.height))
	return tui.SprintScreen(s.View(), tui.WithWidth(s.width), tui.WithHeight(s.height)).Text()
}

// TestPaneVisualEvidence exports pane frames for review, like
// TestTerminalVisualEvidence.
func TestPaneVisualEvidence(t *testing.T) {
	out := os.Getenv("DASHI_TERMINAL_EVIDENCE")
	if out == "" {
		t.Skip("set DASHI_TERMINAL_EVIDENCE to export frames")
	}
	for _, c := range []struct {
		name          string
		width, height int
		details, zoom bool
	}{
		{"panes-two", 120, 40, false, false},
		{"panes-three", 120, 40, true, false},
		{"panes-zoom", 120, 40, true, true},
		{"panes-tabs", 80, 24, true, false},
		{"panes-tabs", 40, 16, true, false},
	} {
		name := fmt.Sprintf("%s-%dx%d", c.name, c.width, c.height)
		s := sidePaneFixture(t, c.width, c.height, c.details)
		if c.zoom {
			s.panes.toggleZoom(s.panes.active)
		}
		frameText(s)
		saveTerminalFrame(t, out, name, s.View(), c.width, c.height)
		s.noColor = true
		saveTerminalFrame(t, out, strings.Replace(name, "panes-", "panes-no-color-", 1), s.View(), c.width, c.height)
	}
}

func TestOnePaneDrawsNoChrome(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}, {40, 16}} {
		s := terminalVisualFixture(t, "success", size[0], size[1])
		before := frameText(s)
		for _, mark := range []string{activeMark, "│", "main ·"} {
			if strings.Contains(before, mark) {
				t.Fatalf("%dx%d single pane shows %q:\n%s", size[0], size[1], mark, before)
			}
		}
		// Opening and closing a pane leaves the frame as it was, apart
		// from the one-time key hint.
		s.panes.hinted = true
		openTestPanes(s, true)
		s.closeSidePanes()
		if after := frameText(s); after != before {
			t.Fatalf("%dx%d frame changed after panes closed:\n%s\nwant:\n%s", size[0], size[1], after, before)
		}
	}
}

func TestSideBySidePanesHaveTitlesAndAGutter(t *testing.T) {
	s := sidePaneFixture(t, 120, 40, true)
	got := frameText(s)
	rows := strings.Split(got, "\n")
	for _, want := range []string{"   main", activeMark + " details · /tasks", "   t_k3f2 · explore · running", "Reading the composer", "t_k3f2 explore running", "I’ll inspect"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	// Main keeps about 60% of the width; the gutter line sits right of it.
	x, _ := cellOf(t, rows, "│")
	if x < 68 || x > 76 {
		t.Fatalf("gutter at column %d, want about 72:\n%s", x, got)
	}
	if !strings.Contains(got, "─") {
		t.Fatalf("no gutter between the right column's rows:\n%s", got)
	}
	// The composer stays full width below the panes.
	if _, y := cellOf(t, rows, "Ask Dashi"); strings.Contains(rows[y], "│") {
		t.Fatalf("composer row shares the split: %q", rows[y])
	}
	// Each pane's footer fits the pane, not the terminal.
	if w := s.paneWidth("task:t_k3f2"); w >= 60 || w <= 0 {
		t.Fatalf("task pane width=%d", w)
	}

	frame := tui.SprintScreen(s.View(), tui.WithWidth(120), tui.WithHeight(40))
	tx, ty := cellOf(t, rows, activeMark+" details")
	mx, my := cellOf(t, rows, "main")
	active, muted := frame.Cell(tx, ty).Style.Foreground, frame.Cell(mx, my).Style.Foreground
	if active == muted {
		t.Fatal("active and inactive titles share a color")
	}
	s.panes.get("task:t_k3f2").waiting = true
	rows = strings.Split(frameText(s), "\n")
	wx, wy := cellOf(t, rows, "needs approval")
	waiting := tui.SprintScreen(s.View(), tui.WithWidth(120), tui.WithHeight(40)).Cell(wx, wy).Style.Foreground
	if waiting == active || waiting == muted {
		t.Fatal("a waiting task's title does not stand out")
	}
}

func TestZoomDrawsOnePaneWithItsTitle(t *testing.T) {
	s := sidePaneFixture(t, 120, 40, true)
	s.panes.toggleZoom("task:t_k3f2")
	got := frameText(s)
	if !strings.Contains(got, activeMark+" t_k3f2 · zoomed · explore · running · 3 model calls · $0.0123") || strings.Contains(got, "I’ll inspect") || strings.Contains(got, "│") {
		t.Fatalf("zoom frame:\n%s", got)
	}
	// The real layout survives zoom.
	if len(s.panes.leaves()) != 3 {
		t.Fatal("zoom changed the layout")
	}
}

func TestNarrowModeShowsTabs(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {40, 16}, {119, 30}} {
		s := sidePaneFixture(t, size[0], size[1], true)
		got := frameText(s)
		rows := strings.Split(got, "\n")
		if !strings.HasPrefix(rows[0], " main · t_k3f2 · "+activeMark+" details") {
			t.Fatalf("%dx%d tab strip=%q", size[0], size[1], rows[0])
		}
		if strings.Contains(got, "I’ll inspect") || strings.Contains(got, "│") || !strings.Contains(got, "t_k3f2 explore running") {
			t.Fatalf("%dx%d must show only the active pane:\n%s", size[0], size[1], got)
		}
		for _, want := range []string{"Ask Dashi", "/help"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%dx%d lost %q:\n%s", size[0], size[1], want, got)
			}
		}
	}
	// At 120 columns the split shows.
	if got := frameText(sidePaneFixture(t, 120, 30, true)); !strings.Contains(got, "│") {
		t.Fatalf("120 columns folded:\n%s", got)
	}
}

func TestNarrowTabsCollapseIntoMore(t *testing.T) {
	s := sidePaneFixture(t, 40, 16, true)
	for _, id := range []string{"t_aaaaaaaa", "t_bbbbbbbb", "t_cccccccc"} {
		s.openPane(s.newPane("task:"+id, paneTask, id))
	}
	s.activatePane(mainPaneID)
	shown, hidden := s.tabs(40)
	if len(hidden) == 0 || shown[0].id != mainPaneID {
		t.Fatalf("shown=%v hidden=%v", shown, hidden)
	}
	rows := strings.Split(frameText(s), "\n")
	if !strings.Contains(rows[0], fmt.Sprintf("+%d", len(hidden))) {
		t.Fatalf("tab strip=%q", rows[0])
	}
	if w := len([]rune(strings.TrimRight(rows[0], " "))); w > 40 {
		t.Fatalf("tab strip too wide: %q", rows[0])
	}
}

func TestPanesKeepNoColorMark(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}} {
		s := sidePaneFixture(t, size[0], size[1], true)
		s.panes.get("task:t_k3f2").waiting = true
		s.noColor = true
		got := frameText(s)
		frame := tui.SprintScreen(s.View(), tui.WithWidth(size[0]), tui.WithHeight(size[1]))
		if !strings.Contains(got, activeMark+" details") || !strings.Contains(got, "needs approval") {
			t.Fatalf("%dx%d no-color frame lost the mark or the warning:\n%s", size[0], size[1], got)
		}
		for y := range size[1] {
			for x := range size[0] {
				if st := frame.Cell(x, y).Style; st.Foreground.Type != 0 || st.Background.Type != 0 {
					t.Fatalf("colored cell at %d,%d", x, y)
				}
			}
		}
	}
}
