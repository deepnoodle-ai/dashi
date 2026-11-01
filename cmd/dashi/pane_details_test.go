package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

// detailsText is the plain text of the details pane's blocks.
func detailsText(s *screen) string {
	p := s.panes.get(detailsPaneID)
	if p == nil {
		return ""
	}
	var lines []string
	for _, b := range p.view.tr.Blocks {
		lines = append(lines, FormatBlock(b))
	}
	return strings.Join(lines, "\n")
}

// mainScrolledUp is a 160×40 screen whose main pane holds enough text to
// scroll, scrolled up from the bottom.
func mainScrolledUp(t *testing.T) *screen {
	t.Helper()
	s := terminalVisualFixture(t, "success", 160, 40)
	for i := range 60 {
		s.Emit(Block{ID: fmt.Sprintf("m%d", i), Kind: KindAssistant, Text: fmt.Sprintf("main line %d", i), Done: true})
	}
	s.drain()
	frameText(s)
	s.main().vp.ScrollBy(-20)
	frameText(s)
	return s
}

func TestDetailsPaneReplacesItsContentAndKeepsMainInPlace(t *testing.T) {
	s := mainScrolledUp(t)
	s.clock = func() time.Time { return time.Date(2026, 10, 31, 20, 31, 5, 0, time.Local) }
	top, _ := s.main().vp.Anchor()
	s.ShowDetails(detailsUpdate{title: "/tasks", blocks: []Block{noticeBlock("Tasks:\n  t_1 explore")}})
	s.drain()
	frame := frameText(s)
	// The title says what the pane holds and when it was captured.
	if s.panes.active != detailsPaneID || !strings.Contains(frame, activeMark+" details · /tasks · 20:31") {
		t.Fatalf("the details pane did not open with focus:\n%s", frame)
	}
	if now, _ := s.main().vp.Anchor(); now != top || s.main().vp.Follow {
		t.Fatalf("main moved from item %d to %d", top, now)
	}
	// A later part of the same report adds to it.
	s.ShowDetails(detailsUpdate{title: "/tasks", blocks: []Block{noticeBlock("Session spend")}, add: true})
	s.drain()
	if got := detailsText(s); !strings.Contains(got, "t_1 explore") || !strings.Contains(got, "Session spend") {
		t.Fatalf("details=%q", got)
	}
	// New details replace the old ones; there is one details pane.
	s.activatePane(mainPaneID)
	s.ShowDetails(detailsUpdate{title: "/settings", blocks: []Block{noticeBlock("Next turn: fixture")}})
	s.drain()
	if got := detailsText(s); got != "Next turn: fixture" || s.panes.count() != 2 || s.panes.active != detailsPaneID {
		t.Fatalf("details=%q count=%d active=%s", got, s.panes.count(), s.panes.active)
	}
	// A late part of a report the pane no longer shows is dropped.
	s.ShowDetails(detailsUpdate{title: "/tasks", blocks: []Block{noticeBlock("late spend")}, add: true})
	s.drain()
	if strings.Contains(detailsText(s), "late spend") {
		t.Fatal("a late /tasks part landed in /settings")
	}
}

func TestInspectionCommandsUseTheDetailsPane(t *testing.T) {
	h := newScreenHarness(t, 0)
	h.renderAt(160, 40)
	h.submit("/status")
	h.waitFor(func() bool {
		p := h.scr.panes.get(detailsPaneID)
		return p != nil && p.title == "/status" && strings.Contains(detailsText(h.scr), "Recorded snapshot")
	})
	id := h.app.eng.Session().ID
	h.check(func() bool { h.scr.activatePane(mainPaneID); return true })
	h.submit("/status " + id)
	h.waitFor(func() bool {
		p := h.scr.panes.get(detailsPaneID)
		return p.title == "/status "+id && strings.Contains(detailsText(h.scr), "Recorded snapshot")
	})
	h.check(func() bool {
		if strings.Count(detailsText(h.scr), "Recorded snapshot") != 1 || h.scr.panes.active != detailsPaneID {
			t.Error("a second /status did not replace the first or take focus")
		}
		for _, b := range h.scr.main().tr.Blocks {
			if strings.Contains(b.Text, "Recorded snapshot") || strings.Contains(b.Text, "Loading recorded status") {
				t.Error("inspection output also printed in main")
			}
		}
		return true
	})
	// /help is inspection output too.
	h.submit("/help")
	h.waitFor(func() bool {
		p := h.scr.panes.get(detailsPaneID)
		return p.title == "/help" && strings.Contains(detailsText(h.scr), "Page Up/Down  scroll the focused pane")
	})
	h.submit("/status " + id)
	h.waitFor(func() bool { return h.scr.panes.get(detailsPaneID).title == "/status "+id })
	// With nothing to list, /tasks says so in main and leaves the pane.
	h.submit("/tasks")
	h.waitFor(func() bool { return strings.Contains(dialogTranscriptText(h.scr), "No delegated tasks") })
	h.check(func() bool {
		if h.scr.panes.get(detailsPaneID).title != "/status "+id {
			t.Error("an empty /tasks replaced the details")
		}
		return true
	})
}

func TestOpenInSidePaneShowsAToolRowOrADiff(t *testing.T) {
	s := mainScrolledUp(t)
	top, _ := s.main().vp.Anchor()
	s.main().apply(Block{ID: "t:1", Kind: KindTool, Name: "read_file", Arg: "go.mod", State: StateOK, Summary: "12 lines", Detail: "module example\n\ngo 1.27", Done: true})
	s.main().apply(Block{ID: "diff:1", Kind: KindDiff, Lines: []string{"diff --git a/x.go b/x.go", "+added line"}, Done: true})
	n := len(s.main().tr.Blocks)
	choose := func(i int) {
		t.Helper()
		for _, item := range s.transcriptMenuItems(s.main(), i) {
			if item.Label == "Show in details" {
				item.Action()
				return
			}
		}
		t.Fatalf("no Show in details for item %d", i)
	}
	choose(n - 2)
	frame := frameText(s)
	if !strings.Contains(frame, "details · read_file · go.mod") || !strings.Contains(frame, "module example") {
		t.Fatalf("the tool row did not open expanded:\n%s", frame)
	}
	if now, _ := s.main().vp.Anchor(); now != top {
		t.Fatal("main moved")
	}
	choose(n - 1)
	frame = frameText(s)
	if !strings.Contains(frame, "details · diff · x.go") || strings.Contains(frame, "module example") {
		t.Fatalf("the diff did not replace the tool row:\n%s", frame)
	}
	// A running tool row has no complete output to open.
	s.main().apply(Block{ID: "t:2", Kind: KindTool, Name: "run", Arg: "make", State: StateRunning, Detail: "partial"})
	for _, item := range s.transcriptMenuItems(s.main(), len(s.main().tr.Blocks)-1) {
		if item.Label == "Show in details" {
			t.Fatal("a running tool row offers Show in details")
		}
	}
}

func TestDetailsCommandsPrintInInlineMode(t *testing.T) {
	ui := newRecorderUI()
	c := &Controller{ui: ui}
	c.showDetails("/settings", noticeBlock("Next turn: fixture"))
	c.addDetails("/tasks", noticeBlock("spend"))
	if c.hasDetails() || len(ui.log) != 2 || ui.log[0].Text != "Next turn: fixture" {
		t.Fatalf("blocks=%+v", ui.log)
	}
}

// The pane title menu of a task pane offers its details, which run
// /tasks ID.
func TestTaskPaneTitleMenuOffersTaskDetails(t *testing.T) {
	s, _ := panesOn(t)
	rows := strings.Split(frameText(s), "\n")
	x, y := cellOf(t, rows, "t_k3f2 · running")
	click(s, tui.MouseButtonRight, x, y)
	if items := s.transcriptMenu.Items(); len(items) == 0 || items[0].Label != "Task details" {
		t.Fatalf("menu=%v", items)
	}
	s.transcriptMenu.Close()
	// The details and main panes have no task, so no Task details.
	x, y = cellOf(t, rows, "details · /tasks")
	click(s, tui.MouseButtonRight, x, y)
	if items := s.transcriptMenu.Items(); items[0].Label == "Task details" {
		t.Fatal("the details pane offers Task details")
	}
}
