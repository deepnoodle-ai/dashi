package main

import (
	"fmt"
	"strings"
)

// The details pane is the one reused side pane for inspection output:
// /tasks, /tasks ID, /status, /settings, and a tool row or diff opened
// from its menu. New details replace the old ones, the pane takes focus,
// and main keeps its scroll position. Inline and plain modes have no
// details pane; there the same output prints as notices.

// detailsUpdate is content for the details pane.
type detailsUpdate struct {
	// title says what the pane shows, such as "/tasks".
	title  string
	blocks []Block
	// add appends blocks to what the pane shows, but only while it still
	// shows title; otherwise they are dropped. Without add, blocks
	// replace the pane's content.
	add bool
}

// panesUI is a UI that can have panes. Only fullscreen has them: Panes
// reports false in inline mode, which embeds the fullscreen screen, and
// the Controller then prints what it would show in a pane, as it did
// before panes existed.
type panesUI interface {
	Panes() bool
	ShowDetails(detailsUpdate)
	OpenTask(id string)
	PaneCommand(arg string)
}

// panes returns the UI's panes, or nil where it has none.
func (c *Controller) panes() panesUI {
	if ui, ok := c.ui.(panesUI); ok && ui.Panes() {
		return ui
	}
	return nil
}

// Panes reports whether the screen draws panes: false in inline mode. It
// implements panesUI.
func (s *screen) Panes() bool { return !s.inline }

// ShowDetails queues content for the details pane. It implements panesUI.
func (s *screen) ShowDetails(d detailsUpdate) {
	s.enqueue(queued{details: &d})
}

// showDetails shows d in the details pane on the loop, opening the pane
// and focusing it unless d adds to it.
func (s *screen) showDetails(d detailsUpdate) {
	p := s.panes.get(detailsPaneID)
	if d.add {
		if p == nil || p.title != d.title {
			return
		}
		for _, b := range d.blocks {
			p.view.apply(b)
		}
		return
	}
	if p == nil {
		p = s.newPane(detailsPaneID, paneDetails, "details")
	} else {
		p.view.reset()
		if s.transcriptMenuPane == detailsPaneID {
			s.transcriptMenu.Close()
		}
	}
	p.title = d.title
	for _, b := range d.blocks {
		p.view.apply(b)
	}
	p.view.vp.ScrollToTop()
	if !s.openPane(p) {
		// No room: the details print in main, as they did before panes.
		for _, b := range d.blocks {
			s.main().apply(b)
		}
	}
}

// openInDetails shows block i of pane from in the details pane: a tool row
// expanded, or a diff.
func (s *screen) openInDetails(from *transcriptPane, i int) {
	if i < 0 || i >= len(from.tr.Blocks) {
		return
	}
	b := from.tr.Blocks[i]
	b.Parent = ""
	s.showDetails(detailsUpdate{title: detailsTitle(b), blocks: []Block{b}})
	if d := s.panes.get(detailsPaneID); d != nil && b.Kind == KindTool {
		d.view.toggleTool(0)
	}
}

// detailsTitle names a tool row or diff in the details pane's title row.
func detailsTitle(b Block) string {
	if b.Kind == KindDiff {
		if len(b.Lines) > 0 && strings.HasPrefix(b.Lines[0], "diff --git ") {
			return "diff · " + diffPath(b.Lines[0])
		}
		return "diff"
	}
	return joinNonEmpty(" · ", b.Name, truncate(firstLine(b.Arg), 60))
}

// showDetails shows the output of the inspection command title in the
// details pane, or prints it as notices where the UI has none.
func (c *Controller) showDetails(title string, blocks ...Block) {
	if ui := c.panes(); ui != nil {
		ui.ShowDetails(detailsUpdate{title: title, blocks: blocks})
		return
	}
	for _, b := range blocks {
		c.emit(b)
	}
}

// addDetails adds output that arrives later, such as the spend after the
// /tasks list, to the details pane while it still shows title. Where the
// UI has no details pane, it prints as notices.
func (c *Controller) addDetails(title string, blocks ...Block) {
	if ui := c.panes(); ui != nil {
		ui.ShowDetails(detailsUpdate{title: title, blocks: blocks, add: true})
		return
	}
	for _, b := range blocks {
		c.emit(b)
	}
}

// hasDetails reports whether the UI shows inspection output in a details
// pane.
func (c *Controller) hasDetails() bool { return c.panes() != nil }

// noticeBlock is a notice with formatted text.
func noticeBlock(format string, args ...any) Block {
	return Block{Kind: KindNotice, Text: fmt.Sprintf(format, args...), Done: true}
}
