package main

import (
	"fmt"
	"image"
	"strings"

	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// Hit areas of the pane chrome. A title row's key is paneTitleArea plus
// the pane ID; a tab's is paneTabArea plus the pane ID. A title row's ×
// records its own cells in pane.closer.
const (
	paneTitleArea = "pane-title:"
	paneTabArea   = "pane-tab:"
	paneMoreArea  = "pane-tabs-more"
)

// moreGap is how many cells left of the "more below" mark are cleared.
const moreGap = 3

// activeMark starts the active pane's title row and tab, so the active pane
// stays clear without color.
const activeMark = "▸"

// narrow reports whether the panes fold to the active pane and a tab strip.
// With only main open there is nothing to fold.
func (s *screen) narrow() bool {
	return s.panes.count() > 1 && s.width > 0 && s.width < narrowWidth
}

// paneArea draws the panes. With one pane it draws exactly what the screen
// drew before panes existed: no title row, gutter, or tab strip. Zoom and
// narrow mode draw a one-leaf layout of the pane they show, so the real
// layout keeps its tree and weights.
func (s *screen) paneArea() tui.View {
	ps := &s.panes
	switch {
	case ps.count() < 2:
		ps.drawn = &ps.split
		return tui.Split(&ps.split, s.paneView)
	case s.narrow():
		ps.single = tui.SplitState{Root: &tui.SplitNode{ID: ps.active}}
		ps.drawn = &ps.single
		return tui.Stack(s.tabStrip(), tui.Split(&ps.single, s.paneView)).Gap(0)
	case ps.zoom != "":
		ps.single = tui.SplitState{Root: &tui.SplitNode{ID: ps.zoom}}
		ps.drawn = &ps.single
		return tui.Split(&ps.single, s.paneView)
	}
	ps.drawn = &ps.split
	return tui.Split(&ps.split, s.paneView).GutterStyle(faintStyle).ActiveGutterStyle(accentStyle)
}

// paneView draws the pane with this ID for the split layout: its title row,
// when panes sit side by side, and its transcript.
func (s *screen) paneView(id string) tui.View {
	p := s.panes.get(id)
	if p == nil {
		return nil
	}
	// A pane with another row under it keeps its last row blank, so its
	// turn rule never sits right on the faint line between the rows.
	bottom := 0
	if s.panes.drawn == &s.panes.split && s.panes.zoom == "" && !s.narrow() && s.panes.rowBelow(id) {
		bottom = 1
	}
	var content tui.View = tui.PaddingLTRB(1, 0, 1, bottom, tui.Viewport(&p.view.vp, p.view).Gap(1))
	if s.panes.count() > 1 && !s.narrow() {
		content = tui.Stack(s.hits.Area(paneTitleArea+id, s.paneTitle(p)), content).Gap(0)
	}
	return tui.ZStack(content, s.moreBelow(p)).Align(tui.AlignLeft)
}

// moreBelow draws, over the bottom row of pane p's viewport, how many lines are below
// the view when it is scrolled up, such as "↓ 12 more · Ctrl+End". It is
// part of the pane rather than a footer row, so scrolling never changes
// the height of the panes. It reads the viewport after the viewport drew,
// and records where it drew, since a click on it returns to live output.
func (s *screen) moreBelow(p *pane) tui.View {
	active := p.id == s.panes.active
	return tui.CanvasContext(func(ctx *tui.RenderContext) {
		vp := &p.view.vp
		p.view.more = image.Rectangle{}
		if vp.AtBottom || vp.LinesBelow <= 0 {
			return
		}
		text := fmt.Sprintf(" ↓ %d more ", vp.LinesBelow)
		if active {
			text += "· Ctrl+End "
		}
		text = runewidth.Truncate(text, vp.Width, "")
		w := runewidth.StringWidth(text)
		// The mark ends at the viewport's right edge on its bottom row.
		p.view.more = image.Rect(vp.X+vp.Width-w, vp.Y+vp.Height-1, vp.X+vp.Width, vp.Y+vp.Height)
		at := p.view.more.Min.Sub(ctx.AbsoluteBounds().Min)
		// A few blank cells and the prompt's band set the mark apart from
		// the text it covers, so it does not read as part of that line.
		if gap := min(moreGap, at.X-(vp.X-ctx.AbsoluteBounds().Min.X)); gap > 0 {
			ctx.PrintStyled(at.X-gap, at.Y, strings.Repeat(" ", gap), tui.NewStyle())
		}
		ctx.PrintStyled(at.X, at.Y, text, mutedStyle.WithBackground(promptBackground))
	})
}

// paneTitle is a pane's one-line title row, such as
// "▸ t_k3f2 · running · $0.0123 · 3 model calls · explore": warning when
// its task waits for the person, accent when active, muted otherwise.
// "needs approval" takes the state's place. A zoomed pane's title ends
// with how to unzoom, and every side pane's with a × that closes it; both
// stay when the rest is cut.
func (s *screen) paneTitle(p *pane) tui.View {
	active := p.id == s.panes.active
	mark := " "
	if active {
		mark = activeMark
	}
	text := fmt.Sprintf(" %s %s", mark, joinNonEmpty(" · ", p.label, s.paneTitleText(p)))
	suffix := ""
	if s.panes.zoom == p.id {
		suffix = " · zoomed · Alt+Z"
	}
	style := mutedStyle
	switch {
	case p.waiting:
		style = warningStyle
	case active:
		style = accentStyle
	}
	side := p.kind != paneMain
	return tui.CanvasContext(func(ctx *tui.RenderContext) {
		width, _ := ctx.Size()
		closer := ""
		p.closer = image.Rectangle{}
		if side && width > 8 {
			closer = " × "
			at := ctx.AbsoluteBounds().Min
			p.closer = image.Rect(at.X+width-3, at.Y, at.X+width, at.Y+1)
			ctx.PrintStyled(width-3, 0, closer, mutedStyle)
		}
		room := max(1, width-runewidth.StringWidth(suffix)-len([]rune(closer)))
		ctx.PrintStyled(0, 0, runewidth.Truncate(text, room, "…")+suffix, style)
	}).Height(1)
}

// paneTitleText is what a pane's title row says after its name: the state,
// or "needs approval" in its place, then the title, the subagent, and the
// time the details were captured.
func (s *screen) paneTitleText(p *pane) string {
	if p == nil {
		return ""
	}
	state := p.state
	if p.waiting {
		state = "needs approval"
	}
	parts := []string{state, p.title}
	if p.kind == paneTask {
		parts = append(parts, p.subagent)
	}
	return joinNonEmpty(" · ", append(parts, p.at)...)
}

// paneTab is one tab of the narrow-mode strip.
type paneTab struct {
	id, text string
}

// tabs lays out the strip for this width: the tabs that fit, in layout
// order, and the panes that do not, which the +N entry lists. The active
// tab always shows. Each tab's text has a space on each side, so tabs sit
// apart without a separator and the active tab's reverse band frames it.
func (s *screen) tabs(width int) (shown []paneTab, hidden []string) {
	ps := &s.panes
	leaves := ps.leaves()
	all := make([]paneTab, len(leaves))
	for i, id := range leaves {
		p := ps.get(id)
		text := " " + p.label + " "
		if p.waiting {
			text = " " + p.label + " needs approval "
		}
		all[i] = paneTab{id: id, text: text}
	}
	total := func(tabs []paneTab, more int) int {
		w := 1 // the leading space
		for _, t := range tabs {
			w += runewidth.StringWidth(t.text)
		}
		if more > 0 {
			w += len(fmt.Sprintf(" +%d ", more))
		}
		return w
	}
	if total(all, 0) <= width {
		return all, nil
	}
	keep := map[string]bool{ps.active: true}
	for _, t := range all {
		if keep[t.id] {
			continue
		}
		keep[t.id] = true
		var trial []paneTab
		for _, u := range all {
			if keep[u.id] {
				trial = append(trial, u)
			}
		}
		if total(trial, len(all)-len(trial)) > width {
			delete(keep, t.id)
		}
	}
	for _, t := range all {
		if keep[t.id] {
			shown = append(shown, t)
		} else {
			hidden = append(hidden, t.id)
		}
	}
	return shown, hidden
}

// tabStrip is the one-line strip above the active pane in narrow mode: the
// tabs, such as " main  t_k3f2  details ", with the active one in reverse,
// and on the right what the active pane's title row would say after its
// name, such as "running · $0.0123 · 3 model calls". A click on a tab
// activates its pane; +N opens a menu of the panes that did not fit.
func (s *screen) tabStrip() tui.View {
	shown, hidden := s.tabs(s.width)
	views := []tui.View{tui.Text(" ")}
	for _, t := range shown {
		style := mutedStyle
		switch {
		case t.id == s.panes.active && s.panes.get(t.id).waiting:
			style = warningStyle.WithReverse()
		case t.id == s.panes.active:
			style = accentStyle.WithReverse()
		case s.panes.get(t.id).waiting:
			style = warningStyle
		}
		views = append(views, s.hits.Area(paneTabArea+t.id, tui.Text("%s", t.text).Style(style)))
	}
	if len(hidden) > 0 {
		views = append(views, s.hits.Area(paneMoreArea, tui.Text(" +%d ", len(hidden)).Style(accentStyle)))
	}
	used := 0
	for _, t := range shown {
		used += runewidth.StringWidth(t.text)
	}
	if len(hidden) > 0 {
		used += len(fmt.Sprintf(" +%d ", len(hidden)))
	}
	// The active pane's title fills what is left, cut from the right.
	if room := s.width - used - 3; room > 4 {
		if title := s.paneTitleText(s.panes.get(s.panes.active)); title != "" {
			title = runewidth.Truncate(title, room, "…")
			pad := max(1, s.width-used-1-runewidth.StringWidth(title)-1)
			style := mutedStyle
			if s.panes.get(s.panes.active).waiting {
				style = warningStyle
			}
			views = append(views, tui.Text("%s", strings.Repeat(" ", pad)), tui.Text("%s", title).Style(style))
		}
	}
	return tui.Group(views...)
}
