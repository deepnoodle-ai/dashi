package main

import (
	"slices"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func testPaneSet() paneSet {
	return newPaneSet(newTranscriptPane(mainPaneID, func() int { return 80 }, func() string { return "" }))
}

func testPane(id string, kind paneKind) *pane {
	return &pane{id: id, kind: kind, label: id, view: newTranscriptPane(id, func() int { return 40 }, func() string { return "" })}
}

func TestPaneSetOpenPutsMainLeftAndDetailsLast(t *testing.T) {
	ps := testPaneSet()
	if _, ok := ps.open(testPane(detailsPaneID, paneDetails), 0); !ok {
		t.Fatal("details did not open")
	}
	root := ps.split.Root
	if root.Axis != tui.SplitColumns || root.Children[0].ID != mainPaneID || root.Children[0].Weight != mainWeight || root.Children[1].Weight != sideWeight {
		t.Fatalf("first open: main must keep 3:2, got %+v", root)
	}
	ps.open(testPane("task:a", paneTask), 0)
	ps.open(testPane("task:b", paneTask), 0)
	if got := ps.leaves(); !slices.Equal(got, []string{mainPaneID, "task:a", "task:b", detailsPaneID}) {
		t.Fatalf("leaves=%v", got)
	}
	if ps.split.Root.Children[1].Axis != tui.SplitRows || ps.active != "task:b" {
		t.Fatalf("right column must be rows and the new pane active: %+v active=%s", ps.split.Root.Children[1], ps.active)
	}
	// Opening an open pane activates it and adds nothing.
	ps.open(testPane("task:a", paneTask), 0)
	if ps.active != "task:a" || len(ps.leaves()) != 4 {
		t.Fatalf("reopen: active=%s leaves=%v", ps.active, ps.leaves())
	}
}

func TestPaneSetCloseRepairsActiveAndRestoresOneLeaf(t *testing.T) {
	ps := testPaneSet()
	ps.open(testPane("task:a", paneTask), 0)
	ps.open(testPane(detailsPaneID, paneDetails), 0)
	closed := false
	ps.get(detailsPaneID).onClose = func() { closed = true }
	if ps.close(mainPaneID) {
		t.Fatal("main closed")
	}
	ps.toggleZoom(detailsPaneID)
	if !ps.close(detailsPaneID) || !closed {
		t.Fatal("details did not close")
	}
	if ps.active != mainPaneID || ps.zoom != "" {
		t.Fatalf("active=%q zoom=%q after closing the active, zoomed pane", ps.active, ps.zoom)
	}
	ps.close("task:a")
	if root := ps.split.Root; !root.IsLeaf() || root.ID != mainPaneID {
		t.Fatalf("last close must restore one leaf, got %+v", root)
	}
}

func TestPaneSetRoomMaking(t *testing.T) {
	ps := testPaneSet()
	// 17 lines hold two rows of eight and a gutter, not three.
	const height = 17
	ps.open(testPane("task:a", paneTask), height)
	ps.open(testPane("task:b", paneTask), height)
	if _, ok := ps.open(testPane("task:c", paneTask), height); ok {
		t.Fatal("opened with no room and no ended pane")
	}
	ps.get("task:a").ended = true
	ps.get("task:b").ended, ps.get("task:b").waiting = true, true
	closed, ok := ps.open(testPane("task:c", paneTask), height)
	if !ok || closed == nil || closed.id != "task:a" {
		t.Fatalf("closed=%v ok=%v, want task:a closed", closed, ok)
	}
	// The active pane never closes to make room, nor one that waits.
	ps.get("task:c").ended = true
	if _, ok := ps.open(testPane("task:d", paneTask), height); ok {
		t.Fatal("closed the active or a waiting pane to make room")
	}
	if !columnFits(1, 8) || columnFits(1, 7) || columnFits(3, 25) || !columnFits(3, 26) || !columnFits(9, 0) {
		t.Fatal("columnFits")
	}
}

func TestPaneSetZoomEqualizeAndStep(t *testing.T) {
	ps := testPaneSet()
	ps.toggleZoom(mainPaneID)
	if ps.zoom != "" {
		t.Fatal("zoomed with only main")
	}
	ps.open(testPane("task:a", paneTask), 0)
	ps.open(testPane(detailsPaneID, paneDetails), 0)
	ps.toggleZoom("task:a")
	if ps.zoom != "task:a" || ps.active != "task:a" {
		t.Fatalf("zoom=%q active=%q", ps.zoom, ps.active)
	}
	ps.toggleZoom("task:a")
	if ps.zoom != "" {
		t.Fatal("second zoom did not unzoom")
	}
	ps.split.Root.Children[0].Weight = 9
	ps.split.Root.Children[1].Children[0].Weight = 5
	ps.equalize()
	right := ps.split.Root.Children[1]
	if ps.split.Root.Children[0].Weight != mainWeight || right.Weight != sideWeight || right.Children[0].Weight != right.Children[1].Weight {
		t.Fatalf("equalize: %+v", ps.split.Root)
	}
	ps.active = detailsPaneID
	if ps.step(1) != mainPaneID || ps.step(-1) != "task:a" {
		t.Fatalf("step wraps: %s %s", ps.step(1), ps.step(-1))
	}
	if ps.find("details") == nil || ps.find("nope") != nil {
		t.Fatal("find")
	}
	ps.closeSides()
	if ps.count() != 1 || ps.active != mainPaneID || ps.hinted {
		t.Fatal("closeSides")
	}
}
