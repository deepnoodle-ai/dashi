package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
)

// mainPaneID names the pane that shows the conversation. It is always
// present.
const mainPaneID = "main"

// transcriptPane is one transcript drawn in a scrollable viewport: the
// blocks, where the viewport is scrolled, which tool rows are expanded, and
// what the selection and the transcript menu act on. It implements
// tui.ViewportItems and tui.ViewportSourceItems. Only the Wonton event loop
// touches it.
//
// The screen owns the panes and everything they share: the composer,
// dialogs, the footer, the clipboard, and the transcript menu itself.
type transcriptPane struct {
	id       string
	tr       Transcript
	vp       tui.ViewportState
	expanded map[int]bool
	// selectionRevision changes whenever the selection may have changed,
	// so a clipboard result clears only the selection it copied.
	selectionRevision uint64
	// menuItem is the item the open transcript menu acts on, or -1, and
	// menuShape is its kind and state at open time.
	menuItem  int
	menuShape blockShape
	// width is the width a turn footer fits; spinner is the glyph a
	// running tool row shows.
	width   func() int
	spinner func() string
}

func newTranscriptPane(id string, width func() int, spinner func() string) *transcriptPane {
	p := &transcriptPane{id: id, menuItem: -1, width: width, spinner: spinner}
	p.vp.Follow = true
	return p
}

// Len implements tui.ViewportItems.
func (p *transcriptPane) Len() int { return len(p.tr.Blocks) }

// Item implements tui.ViewportItems.
func (p *transcriptPane) Item(i int) tui.View {
	if i < 0 || i >= len(p.tr.Blocks) {
		return nil
	}
	b := p.liveBlock(i)
	if b.Kind == KindFooter {
		return footerView(b, p.width())
	}
	return blockView(b)
}

// Source implements tui.ViewportSourceItems: it is the text a selection
// copies. Glyphs, labels, rules, and states are decorations. Every
// non-empty source must match text its view marks with SourceOffset;
// otherwise Wonton cannot map the item, treats it as decoration, and a
// selection skips its text.
func (p *transcriptPane) Source(i int) (string, bool) {
	if i < 0 || i >= len(p.tr.Blocks) {
		return "", true
	}
	b := p.liveBlock(i)
	switch b.Kind {
	case KindTool:
		return toolSource(b), true
	case KindDiff:
		return strings.Join(b.Lines, "\n"), true
	case KindFooter:
		return footerSource(b, p.width()), true
	case KindPending:
		if b.Done {
			return "", true
		}
	}
	return b.Text, true
}

// liveBlock is block i as the live screen draws it. A tool row shows its
// input and output when the user expanded it, and spins while it runs.
func (p *transcriptPane) liveBlock(i int) Block {
	b := p.tr.Blocks[i]
	if b.Kind != KindTool {
		return b
	}
	b.Expanded = p.expanded[i] && toolExpandable(b)
	if toolSpins(b.State) {
		b.Spinner = p.spinner()
	}
	return b
}

// apply adds or updates a block and redraws its item. It reports whether
// the item the transcript menu acts on changed kind or state, since the
// menu's entries were chosen for its old shape.
func (p *transcriptPane) apply(b Block) (menuStale bool) {
	i := p.tr.Apply(b)
	p.vp.Invalidate(i)
	return i == p.menuItem && shapeOf(p.tr.Blocks[i]) != p.menuShape
}

// reset empties the transcript and returns to live output.
func (p *transcriptPane) reset() {
	p.tr = Transcript{}
	p.expanded = nil
	p.vp.InvalidateAll()
	p.vp.Follow = true
}

// spinTools redraws the tool lines that show the spinner. Only the lines
// after the last turn footer can still run.
func (p *transcriptPane) spinTools() {
	for i := len(p.tr.Blocks) - 1; i >= 0; i-- {
		b := p.tr.Blocks[i]
		if b.Kind == KindFooter {
			return
		}
		if b.Kind == KindTool && toolSpins(b.State) {
			p.vp.Invalidate(i)
		}
	}
}

// toggleTool expands or collapses tool row i.
func (p *transcriptPane) toggleTool(i int) {
	if i < 0 || i >= len(p.tr.Blocks) || p.tr.Blocks[i].Kind != KindTool || !toolExpandable(p.tr.Blocks[i]) {
		return
	}
	if p.expanded == nil {
		p.expanded = map[int]bool{}
	}
	p.expanded[i] = !p.expanded[i]
	p.vp.Invalidate(i)
}

// clickTool handles a left click at the viewport position (x, y) on a tool
// row and reports whether it was one. A click anywhere on a collapsed row
// expands it. An expanded row collapses only from its head, so a click in
// the output does not.
func (p *transcriptPane) clickTool(x, y int) bool {
	i, line, ok := p.vp.ItemAt(x, y)
	if !ok || i >= len(p.tr.Blocks) || p.tr.Blocks[i].Kind != KindTool || !toolExpandable(p.tr.Blocks[i]) {
		return false
	}
	if line == 0 || !p.expanded[i] {
		p.toggleTool(i)
	}
	return true
}

// local returns e in the viewport's own coordinates and whether it falls
// inside the viewport. The viewport recorded its screen origin at the last
// render.
func (p *transcriptPane) local(e tui.MouseEvent) (tui.MouseEvent, bool) {
	e.X -= p.vp.X
	e.Y -= p.vp.Y
	return e, e.X >= 0 && e.X < p.vp.Width && e.Y >= 0 && e.Y < p.vp.Height
}

// clearSelection drops the selection, if any.
func (p *transcriptPane) clearSelection() {
	p.selectionRevision++
	p.vp.ClearSelection()
}

// dump writes the plain text of the whole transcript, tool rows expanded.
func (p *transcriptPane) dump(w io.Writer) {
	for _, b := range p.tr.Blocks {
		fmt.Fprintln(w, FormatBlock(printable(fullBlock(b))))
	}
}
