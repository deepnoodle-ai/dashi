package main

import (
	"slices"

	"github.com/deepnoodle-ai/wonton/tui"
)

// Panes are the areas of the fullscreen screen that show a transcript side
// by side: the main pane, which is the conversation and is always present,
// a task pane per watched task, and one details pane. The main pane sits on
// the left; every other pane is a row in the right column, with details
// always the bottom row. The composer, status line, and dialogs stay full
// width below the panes.

// paneKind is what a pane shows.
type paneKind int

const (
	paneMain paneKind = iota
	paneTask
	paneDetails
)

// detailsPaneID names the one details pane.
const detailsPaneID = "details"

const (
	// mainWeight and sideWeight are the shares of the width that main and
	// the right column take: main keeps about 60%.
	mainWeight = 3
	sideWeight = 2
	// paneRowLines is the height, title included, that each row of the
	// right column needs before another row may open.
	paneRowLines = 8
	// paneMinSize is the smallest width or height a gutter drag leaves a
	// pane: a title and three lines.
	paneMinSize = 4
	// narrowWidth is the terminal width below which the panes fold to the
	// active pane and a tab strip.
	narrowWidth = 120
)

// pane is one pane of the screen.
type pane struct {
	id   string // "main", "details", or "task:" + task ID
	kind paneKind
	view *transcriptPane
	// label names the pane in its title row and tab, such as "main" or a
	// task ID; title is what the title row says after it.
	label string
	title string
	// opened orders panes by when they opened, oldest first.
	opened uint64
	// ended and waiting are a task pane's state: the task has ended, or it
	// waits for the person to answer an approval.
	ended   bool
	waiting bool
	// onClose, if set, runs when the pane closes.
	onClose func()
}

// paneSet owns every pane and the layout. Only the loop touches it.
type paneSet struct {
	byID map[string]*pane
	// split is the layout; its leaves are pane IDs.
	split tui.SplitState
	// active is the pane that scroll keys act on. It is not Wonton keyboard
	// focus, which stays on the composer.
	active string
	// zoom is the pane that fills the pane area, or "".
	zoom string
	// hinted reports that the one-time key hint has shown.
	hinted bool
	opens  uint64
	// single is the one-leaf layout drawn for zoom and narrow mode, so the
	// real layout survives them. drawn is the layout drawn last frame:
	// split or single. Positions come from it.
	single tui.SplitState
	drawn  *tui.SplitState
}

func newPaneSet(main *transcriptPane) paneSet {
	ps := paneSet{
		byID:   map[string]*pane{mainPaneID: {id: mainPaneID, kind: paneMain, view: main, label: "main"}},
		split:  tui.SplitState{MinSize: paneMinSize},
		active: mainPaneID,
	}
	ps.split.Append(mainPaneID, tui.SplitColumns)
	return ps
}

// get returns the pane with this ID, or nil.
func (ps *paneSet) get(id string) *pane { return ps.byID[id] }

// leaves returns the pane IDs in layout order: main, then the right column
// top to bottom.
func (ps *paneSet) leaves() []string { return ps.split.Leaves() }

// count is the number of open panes.
func (ps *paneSet) count() int { return len(ps.byID) }

// rightColumn returns the rows of the right column, top to bottom, or nil
// when only main is open.
func (ps *paneSet) rightColumn() []*tui.SplitNode {
	root := ps.split.Root
	if root == nil || root.IsLeaf() || len(root.Children) < 2 {
		return nil
	}
	right := root.Children[1]
	if right.IsLeaf() {
		return []*tui.SplitNode{right}
	}
	return right.Children
}

// columnFits reports whether a right column of this many rows gives each
// row paneRowLines lines in a pane area this tall. An unknown height (zero)
// fits.
func columnFits(rows, height int) bool {
	return height <= 0 || rows*paneRowLines+rows-1 <= height
}

// open adds p and makes it active. height is the pane area's height, used
// to check the right column has room. When it has none, open first closes
// the oldest ended task pane that is neither active nor waiting, and
// returns it as closed. When no pane may close, open adds nothing and
// reports false. Opening a pane that is already open activates it.
func (ps *paneSet) open(p *pane, height int) (closed *pane, ok bool) {
	ps.zoom = ""
	if ps.byID[p.id] != nil {
		ps.active = p.id
		return nil, true
	}
	ps.cancelDrag()
	if rows := ps.rightColumn(); rows != nil && !columnFits(len(rows)+1, height) {
		closed = ps.byID[ps.oldestClosable()]
		if closed == nil {
			return nil, false
		}
		ps.close(closed.id)
	}
	ps.insert(p.id)
	ps.opens++
	p.opened = ps.opens
	ps.byID[p.id] = p
	ps.active = p.id
	return closed, true
}

// insert adds a leaf for id to the layout: a right column of its own when
// only main is open, otherwise a row in the right column above details,
// with the column's average weight.
func (ps *paneSet) insert(id string) {
	rows := ps.rightColumn()
	if rows == nil {
		ps.split.Append(id, tui.SplitColumns)
		ps.split.Root.Children[0].Weight, ps.split.Root.Children[1].Weight = mainWeight, sideWeight
		return
	}
	root := ps.split.Root
	right := root.Children[1]
	rows = slices.Clone(rows)
	if right.IsLeaf() {
		// A lone row's weight was its share against main; as a row it
		// starts even with the new one.
		rows = []*tui.SplitNode{{ID: right.ID, Weight: 1}}
	}
	var sum float64
	for _, r := range rows {
		sum += max(r.Weight, 0)
	}
	weight := sum / float64(len(rows))
	if weight <= 0 {
		weight = 1
	}
	at := len(rows)
	if rows[at-1].ID == detailsPaneID {
		at--
	}
	rows = slices.Insert(rows, at, &tui.SplitNode{ID: id, Weight: weight})
	root.Children[1] = &tui.SplitNode{Axis: tui.SplitRows, Children: rows, Weight: right.Weight}
	// A fresh state drops the gutters measured from the old tree.
	ps.split = tui.SplitState{Root: root, MinSize: paneMinSize}
}

// oldestClosable returns the oldest ended task pane that is neither active
// nor waiting for the person, or "".
func (ps *paneSet) oldestClosable() string {
	var best *pane
	for _, p := range ps.byID {
		if p.kind != paneTask || !p.ended || p.waiting || p.id == ps.active {
			continue
		}
		if best == nil || p.opened < best.opened {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.id
}

// close removes a side pane. It refuses main. Closing the active pane makes
// main active.
func (ps *paneSet) close(id string) bool {
	p := ps.byID[id]
	if p == nil || p.kind == paneMain {
		return false
	}
	ps.cancelDrag()
	ps.split.Remove(id)
	delete(ps.byID, id)
	if ps.active == id {
		ps.active = mainPaneID
	}
	ps.repair()
	if p.onClose != nil {
		p.onClose()
	}
	return true
}

// closeSides closes every side pane, as a new session does.
func (ps *paneSet) closeSides() {
	for _, id := range ps.leaves() {
		if id != mainPaneID {
			ps.close(id)
		}
	}
	ps.active, ps.zoom, ps.hinted = mainPaneID, "", false
}

// toggleZoom zooms pane id, or unzooms it if it is zoomed. With only main
// open there is nothing to zoom.
func (ps *paneSet) toggleZoom(id string) {
	if ps.byID[id] == nil || ps.count() < 2 {
		return
	}
	ps.cancelDrag()
	if ps.zoom == id {
		ps.zoom = ""
		return
	}
	ps.zoom, ps.active = id, id
}

// equalize evens the right column and gives main its share back.
func (ps *paneSet) equalize() {
	ps.split.Equalize()
	if root := ps.split.Root; root != nil && !root.IsLeaf() {
		root.Children[0].Weight, root.Children[1].Weight = mainWeight, sideWeight
	}
}

// step returns the pane n places after the active one in layout order,
// wrapping around.
func (ps *paneSet) step(n int) string {
	leaves := ps.leaves()
	i := max(0, slices.Index(leaves, ps.active))
	return leaves[((i+n)%len(leaves)+len(leaves))%len(leaves)]
}

// activate makes pane id active, if it is open.
func (ps *paneSet) activate(id string) bool {
	if ps.byID[id] == nil {
		return false
	}
	ps.active = id
	return true
}

// repair points active and zoom at panes that are still open.
func (ps *paneSet) repair() {
	if ps.byID[ps.active] == nil {
		ps.active = mainPaneID
	}
	if ps.byID[ps.zoom] == nil {
		ps.zoom = ""
	}
}

// cancelDrag ends a gutter drag, before the layout changes under it.
func (ps *paneSet) cancelDrag() {
	ps.split.HandleMouse(tui.MouseEvent{Type: tui.MouseDragCancel})
}

// find returns the pane that a name from /pane focus names: its ID, its
// label (a task ID), or "details".
func (ps *paneSet) find(name string) *pane {
	if p := ps.byID[name]; p != nil {
		return p
	}
	for _, p := range ps.byID {
		if p.label == name {
			return p
		}
	}
	return nil
}
