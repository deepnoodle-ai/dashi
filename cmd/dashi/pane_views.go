package main

import (
	"fmt"
	"image"
	"strings"

	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// Hit areas of the pane chrome. A title row's key is paneTitleArea plus
// the pane ID; a tab's is paneTabArea plus the pane ID.
const (
	paneTitleArea = "pane-title:"
	paneTabArea   = "pane-tab:"
	paneMoreArea  = "pane-tabs-more"
)

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
	var content tui.View = tui.PaddingLTRB(1, 0, 1, 0, tui.Viewport(&p.view.vp, p.view).Gap(1))
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
		// The prompt's band sets the mark apart from the text under it.
		ctx.PrintStyled(at.X, at.Y, text, mutedStyle.WithBackground(promptBackground))
	})
}

// paneTitle is a pane's one-line title row: warning when its task waits
// for the person, accent when active, muted otherwise.
func (s *screen) paneTitle(p *pane) tui.View {
	active := p.id == s.panes.active
	// What needs the person comes first, so truncation keeps it.
	parts := []string{p.label}
	if p.waiting {
		parts = append(parts, "needs approval")
	}
	if s.panes.zoom == p.id {
		parts = append(parts, "zoomed")
	}
	if p.title != "" {
		parts = append(parts, p.title)
	}
	mark := " "
	if active {
		mark = activeMark
	}
	text := fmt.Sprintf(" %s %s", mark, strings.Join(parts, " · "))
	style := mutedStyle
	switch {
	case p.waiting:
		style = warningStyle
	case active:
		style = accentStyle
	}
	return tui.Text("%s", runewidth.Truncate(text, max(1, s.paneWidth(p.id)), "…")).Style(style)
}

// paneTab is one tab of the narrow-mode strip.
type paneTab struct {
	id, text string
}

// tabs lays out the strip for this width: the tabs that fit, in layout
// order, and the panes that do not, which the +N entry lists. The active
// tab always shows.
func (s *screen) tabs(width int) (shown []paneTab, hidden []string) {
	ps := &s.panes
	leaves := ps.leaves()
	all := make([]paneTab, len(leaves))
	for i, id := range leaves {
		p := ps.get(id)
		text := p.label
		if id == ps.active {
			text = activeMark + " " + text
		}
		if p.waiting {
			text += " needs approval"
		}
		all[i] = paneTab{id: id, text: text}
	}
	const sep = " · "
	total := func(tabs []paneTab, more int) int {
		w := 1 // the leading space
		for i, t := range tabs {
			if i > 0 {
				w += len(sep)
			}
			w += runewidth.StringWidth(t.text)
		}
		if more > 0 {
			w += len(sep) + len(fmt.Sprintf("+%d", more))
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

// tabStrip is the one-line strip above the active pane in narrow mode, such
// as "main · t_k3f2 · details". A click on a tab activates its pane; +N
// opens a menu of the panes that did not fit.
func (s *screen) tabStrip() tui.View {
	shown, hidden := s.tabs(s.width)
	views := []tui.View{tui.Text(" ")}
	for i, t := range shown {
		if i > 0 {
			views = append(views, tui.Text(" · ").Style(faintStyle))
		}
		style := mutedStyle
		switch {
		case s.panes.get(t.id).waiting:
			style = warningStyle
		case t.id == s.panes.active:
			style = accentStyle
		}
		views = append(views, s.hits.Area(paneTabArea+t.id, tui.Text("%s", t.text).Style(style)))
	}
	if len(hidden) > 0 {
		views = append(views, tui.Text(" · ").Style(faintStyle),
			s.hits.Area(paneMoreArea, tui.Text("+%d", len(hidden)).Style(accentStyle)))
	}
	return tui.Group(views...)
}
