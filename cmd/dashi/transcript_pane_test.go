package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func paneFixture() *transcriptPane {
	p := newTranscriptPane("t", func() int { return 80 }, func() string { return "*" })
	p.apply(Block{ID: "a", Kind: KindAssistant, Text: "hello", Done: true})
	p.apply(Block{ID: "ok", Kind: KindTool, Name: "bash", Arg: "go test", Input: "$ go test", State: StateOK, Detail: "PASS", Done: true})
	p.apply(Block{ID: "run", Kind: KindTool, Name: "bash", Arg: "sleep", Input: "$ sleep", State: StateRunning, Lines: []string{"tick"}})
	return p
}

// drawPane renders the pane alone and returns its rows.
func drawPane(p *transcriptPane, width, height int) []string {
	return strings.Split(tui.SprintScreen(tui.Viewport(&p.vp, p).Gap(1), tui.WithWidth(width), tui.WithHeight(height)).Text(), "\n")
}

func TestTranscriptPaneItems(t *testing.T) {
	p := paneFixture()
	if p.Len() != 3 || !p.vp.Follow || p.menuItem != -1 {
		t.Fatalf("len=%d follow=%v menuItem=%d", p.Len(), p.vp.Follow, p.menuItem)
	}
	if p.Item(-1) != nil || p.Item(3) != nil {
		t.Fatal("an item out of range has a view")
	}
	if src, ok := p.Source(3); src != "" || !ok {
		t.Fatalf("source out of range=%q", src)
	}
	if src, _ := p.Source(0); src != "hello" {
		t.Fatalf("message source=%q", src)
	}
	// A running tool row spins with the pane's glyph.
	if b := p.liveBlock(2); b.Spinner != "*" {
		t.Fatalf("spinner=%q", b.Spinner)
	}
}

func TestTranscriptPaneToggleTool(t *testing.T) {
	p := paneFixture()
	p.toggleTool(0) // a message has nothing to expand
	p.toggleTool(9)
	if len(p.expanded) != 0 {
		t.Fatalf("expanded=%v", p.expanded)
	}
	p.toggleTool(1)
	if !p.liveBlock(1).Expanded {
		t.Fatal("tool row did not expand")
	}
	if src, _ := p.Source(1); !strings.Contains(src, "$ go test") {
		t.Fatalf("expanded source=%q", src)
	}
	p.toggleTool(1)
	if p.liveBlock(1).Expanded {
		t.Fatal("tool row did not collapse")
	}
}

func TestTranscriptPaneClickTool(t *testing.T) {
	p := paneFixture()
	rows := drawPane(p, 60, 20)
	x, y := cellOf(t, rows, "Ran a command")
	if p.clickTool(x, 0) {
		t.Fatal("a click on a message counted as a tool row")
	}
	if !p.clickTool(x, y) || !p.expanded[1] {
		t.Fatal("a click on a collapsed row did not expand it")
	}
	rows = drawPane(p, 60, 20)
	// A click in the output keeps the row open; a click on its head closes it.
	ox, oy := cellOf(t, rows, "PASS")
	if !p.clickTool(ox, oy) || !p.expanded[1] {
		t.Fatal("a click in the output collapsed the row")
	}
	if !p.clickTool(x, y) || p.expanded[1] {
		t.Fatal("a click on the head did not collapse the row")
	}
}

func TestTranscriptPaneApplyReportsStaleMenu(t *testing.T) {
	p := paneFixture()
	p.menuItem, p.menuShape = 2, shapeOf(p.tr.Blocks[2])
	if p.apply(Block{ID: "run", Kind: KindTool, Name: "bash", State: StateRunning, Lines: []string{"tick", "tock"}}) {
		t.Fatal("new output alone made the menu stale")
	}
	if p.apply(Block{ID: "other", Kind: KindAssistant, Text: "more"}) {
		t.Fatal("another item made the menu stale")
	}
	if !p.apply(Block{ID: "run", Kind: KindTool, Name: "bash", State: StateOK, Done: true}) {
		t.Fatal("a state change left the menu open")
	}
}

func TestTranscriptPaneResetAndDump(t *testing.T) {
	p := paneFixture()
	var out bytes.Buffer
	p.dump(&out)
	// The dump shows tool rows expanded.
	if !strings.Contains(out.String(), "hello") || !strings.Contains(out.String(), "PASS") {
		t.Fatalf("dump=%q", out.String())
	}
	p.toggleTool(1)
	p.vp.ScrollBy(-5)
	p.vp.Follow = false
	p.reset()
	if p.Len() != 0 || p.expanded != nil || !p.vp.Follow {
		t.Fatalf("len=%d expanded=%v follow=%v", p.Len(), p.expanded, p.vp.Follow)
	}
}

func TestTranscriptPaneLocal(t *testing.T) {
	p := paneFixture()
	drawPane(p, 60, 20)
	p.vp.X, p.vp.Y = 2, 3 // as if drawn at (2, 3)
	if e, inside := p.local(tui.MouseEvent{X: 5, Y: 4}); !inside || e.X != 3 || e.Y != 1 {
		t.Fatalf("local=(%d, %d) inside=%v", e.X, e.Y, inside)
	}
	if _, inside := p.local(tui.MouseEvent{X: 1, Y: 4}); inside {
		t.Fatal("a point left of the pane is inside")
	}
}

func TestScreenHasOneMainPane(t *testing.T) {
	s := newScreen("fixture")
	if got := s.panes.leaves(); !slices.Equal(got, []string{mainPaneID}) {
		t.Fatalf("leaves=%v", got)
	}
	if s.focused() != s.main() || s.main().id != mainPaneID || len(s.paneList()) != 1 {
		t.Fatal("the main pane is not the only, focused pane")
	}
	s.width, s.height = 40, 10
	s.Emit(Block{ID: "a", Kind: KindAssistant, Text: "hello", Done: true})
	s.drain()
	// The pane is under the pointer once drawn.
	tui.SprintScreen(s.paneArea(), tui.WithWidth(40), tui.WithHeight(5))
	if s.paneAt(1, 0) != s.main() {
		t.Fatal("the main pane is not under the pointer")
	}
	if s.main().Len() != 1 {
		t.Fatalf("main len=%d", s.main().Len())
	}
}
