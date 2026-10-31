package main

import (
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
)

// paneKeyHint is the one-time notice the first side pane of a session
// shows.
const paneKeyHint = "Panes: click to focus · Alt+arrows move focus · Alt+Z zooms · Alt+W closes · /pane for more. Typing still goes to the main agent."

// paneUsage is the reply to a /pane command it cannot read.
const paneUsage = "Usage: /pane close|zoom|equalize|next, or /pane focus main|ID|details"

// newPane returns a side pane with an empty transcript.
func (s *screen) newPane(id string, kind paneKind, label string) *pane {
	return &pane{id: id, kind: kind, label: label, view: s.newTranscriptPane(id)}
}

// openPane opens p, or activates it if it is open, and reports whether it
// is open. It shows a notice when it closes a pane to make room, when there
// is no room, and the first time a side pane opens in a session.
func (s *screen) openPane(p *pane) bool {
	if p.view == nil {
		p.view = s.newTranscriptPane(p.id)
	}
	closed, ok := s.panes.open(p, s.paneAreaHeight())
	if !ok {
		s.Emit(Block{Kind: KindNotice, Text: "No room for another pane. Close one first with Alt+W or /pane close.", Done: true})
		return false
	}
	if closed != nil {
		s.paneClosed(closed.id)
		s.Emit(Block{Kind: KindNotice, Text: fmt.Sprintf("Closed the %s pane to make room.", closed.label), Done: true})
	}
	if !s.panes.hinted {
		s.panes.hinted = true
		s.Emit(Block{Kind: KindNotice, Text: paneKeyHint, Done: true})
	}
	s.syncMouseMode()
	return true
}

// closePane closes a side pane. Main cannot close.
func (s *screen) closePane(id string) {
	if id == mainPaneID {
		s.Emit(Block{Kind: KindNotice, Text: "The main pane cannot be closed.", Done: true})
		return
	}
	if s.panes.close(id) {
		s.paneClosed(id)
		s.syncMouseMode()
	}
}

// closeSidePanes closes every side pane, as a new session does.
func (s *screen) closeSidePanes() {
	s.panes.closeSides()
	if s.transcriptMenuPane != mainPaneID {
		s.transcriptMenu.Close()
	}
	s.syncMouseMode()
}

// paneClosed drops what still points at a pane that closed.
func (s *screen) paneClosed(id string) {
	if s.transcriptMenuPane == id {
		s.transcriptMenu.Close()
	}
	if task, ok := strings.CutPrefix(id, taskPaneID("")); ok {
		s.redrawTaskLines(task)
	}
}

// redrawTaskLines redraws the status lines of task id in every pane, since
// they say whether its pane is open.
func (s *screen) redrawTaskLines(id string) {
	for _, p := range s.paneList() {
		for i, b := range p.tr.Blocks {
			if taskOfLine(b) == id {
				p.vp.Invalidate(i)
			}
		}
	}
}

// paneAreaHeight is the height of the pane area at the last frame, or zero
// before the first.
func (s *screen) paneAreaHeight() int {
	d := s.panes.drawn
	if d == nil || d.Root == nil {
		return 0
	}
	h := d.Root.Bounds().Dy()
	if h > 0 && d == &s.panes.single && s.narrow() {
		h++ // the tab strip
	}
	return h
}

// syncMouseMode reports pointer motion without a button held while panes
// sit side by side, so the gutter under the pointer highlights. Otherwise
// it reports drags only, which is far quieter.
func (s *screen) syncMouseMode() {
	if s.runtime == nil || !s.mouseEnabled {
		return
	}
	t := s.runtime.Terminal()
	want := tui.MouseModeDrag
	if s.panes.count() > 1 && s.panes.zoom == "" && !s.narrow() {
		want = tui.MouseModeTracking
	}
	if t.MouseMode() == want {
		return
	}
	t.DisableMouseTracking()
	if want == tui.MouseModeTracking {
		t.EnableMouseTracking()
	} else {
		t.EnableMouseDrag()
	}
}

// PaneCommand runs /pane on the loop. It implements panesUI.
func (s *screen) PaneCommand(arg string) {
	s.TranscriptCommand("/pane", arg)
}

// paneCommand runs /pane close|zoom|equalize|next and /pane focus NAME.
func (s *screen) paneCommand(arg string) {
	ps := &s.panes
	fields := strings.Fields(strings.ToLower(arg))
	notice := func(format string, args ...any) {
		s.Emit(Block{Kind: KindNotice, Text: fmt.Sprintf(format, args...), Done: true})
	}
	if len(fields) == 0 || len(fields) > 2 || (len(fields) == 2) != (fields[0] == "focus") {
		s.Emit(warnBlock("%s", paneUsage))
		return
	}
	switch fields[0] {
	case "close":
		s.closePane(ps.active)
	case "zoom":
		if ps.count() < 2 {
			notice("Only the main pane is open.")
			return
		}
		ps.toggleZoom(ps.active)
	case "equalize":
		ps.equalize()
	case "next":
		if ps.count() < 2 {
			notice("Only the main pane is open.")
			return
		}
		s.activatePane(ps.step(1))
	case "focus":
		p := ps.find(fields[1])
		if p == nil {
			var names []string
			for _, id := range ps.leaves() {
				names = append(names, ps.get(id).label)
			}
			notice("No pane named %s. Open panes: %s.", fields[1], strings.Join(names, ", "))
			return
		}
		s.activatePane(p.id)
	default:
		s.Emit(warnBlock("%s", paneUsage))
		return
	}
	s.syncMouseMode()
}

// activatePane makes pane id active. A zoom on another pane ends, so the
// active pane is always on screen.
func (s *screen) activatePane(id string) {
	ps := &s.panes
	if !ps.activate(id) {
		return
	}
	if ps.zoom != "" && ps.zoom != id {
		ps.zoom = ""
	}
	s.syncMouseMode()
}

// isPaneKey reports whether e is a pane key: Alt with an arrow, Z, or W.
func isPaneKey(e tui.KeyEvent) bool {
	if !e.Alt || e.Ctrl || e.Paste != "" {
		return false
	}
	switch e.Key {
	case tui.KeyArrowLeft, tui.KeyArrowRight, tui.KeyArrowUp, tui.KeyArrowDown:
		return true
	case tui.KeyUnknown:
		switch e.Rune {
		case 'z', 'Z', 'w', 'W':
			return true
		}
	}
	return false
}

// paneKey runs a pane key and reports whether e was one.
func (s *screen) paneKey(e tui.KeyEvent) bool {
	if !isPaneKey(e) {
		return false
	}
	ps := &s.panes
	switch e.Key {
	case tui.KeyArrowLeft:
		s.movePaneFocus(-1, 0)
	case tui.KeyArrowRight:
		s.movePaneFocus(1, 0)
	case tui.KeyArrowUp:
		s.movePaneFocus(0, -1)
	case tui.KeyArrowDown:
		s.movePaneFocus(0, 1)
	default:
		switch e.Rune {
		case 'z', 'Z':
			ps.toggleZoom(ps.active)
		case 'w', 'W':
			if ps.active != mainPaneID {
				s.closePane(ps.active)
			}
		}
	}
	s.syncMouseMode()
	return true
}

// movePaneFocus activates the neighbor of the active pane in a direction.
// It ends zoom first. In narrow mode, or when the layout's positions are
// stale, as just after zoom, left and up step back through the panes in
// layout order and right and down step forward.
func (s *screen) movePaneFocus(dx, dy int) {
	ps := &s.panes
	if ps.count() < 2 {
		return
	}
	_, placed := ps.split.Bounds(ps.active)
	fresh := placed && ps.drawn == &ps.split && !s.narrow()
	if ps.zoom != "" {
		ps.zoom, fresh = "", false
	}
	if !fresh {
		ps.active = ps.step(dx + dy)
		return
	}
	if id, ok := ps.split.Neighbor(ps.active, dx, dy); ok {
		ps.active = id
	}
}

// paneChromeMouse handles a press or click on a title row, a tab, or the
// +N tab, and reports whether it took the event. A left click activates
// the pane; a right click opens the pane menu.
func (s *screen) paneChromeMouse(e tui.MouseEvent) bool {
	if e.Type != tui.MousePress && e.Type != tui.MouseClick {
		return false
	}
	for _, p := range s.paneList() {
		if p.vp.SelectionActive() {
			return false
		}
	}
	area, ok := s.hits.At(e.X, e.Y)
	if !ok {
		return false
	}
	var id string
	switch {
	case strings.HasPrefix(area, paneTitleArea):
		id = strings.TrimPrefix(area, paneTitleArea)
	case strings.HasPrefix(area, paneTabArea):
		id = strings.TrimPrefix(area, paneTabArea)
	case area == paneMoreArea:
		if e.Type == tui.MouseClick && e.Button == tui.MouseButtonLeft {
			s.openHiddenPanesMenu(e.X, e.Y)
		}
		return true
	default:
		return false
	}
	if e.Type == tui.MouseClick {
		switch e.Button {
		case tui.MouseButtonLeft:
			s.activatePane(id)
		case tui.MouseButtonRight:
			s.activatePane(id)
			s.openPaneMenu(id, e.X, e.Y)
		}
	}
	return true
}

// openPaneMenu opens the menu of a pane's title row: Task details on a
// task pane, then Zoom or Unzoom, Equalize, and Close, which main does not
// offer.
func (s *screen) openPaneMenu(id string, x, y int) {
	p := s.panes.get(id)
	if p == nil || !s.mainComposerActive() {
		return
	}
	zoom := "Zoom"
	if s.panes.zoom == id {
		zoom = "Unzoom"
	}
	var items []tui.MenuItem
	// A task pane's full facts open in the details pane, as /tasks ID.
	if p.kind == paneTask && s.ctrl != nil {
		task := p.label
		items = append(items, tui.MenuItem{Label: "Task details", Action: func() { s.ctrl.taskInput("/tasks " + task) }}, tui.MenuSeparator())
	}
	items = append(items,
		tui.MenuItem{Label: zoom, Hint: "Alt+Z", Action: func() { s.panes.toggleZoom(id); s.syncMouseMode() }},
		tui.MenuItem{Label: "Equalize", Action: s.panes.equalize},
		tui.MenuItem{Label: "Close", Hint: "Alt+W", Disabled: id == mainPaneID, Action: func() { s.closePane(id) }},
	)
	s.transcriptMenuPane, p.view.menuItem = id, -1
	s.transcriptMenu.Open(x, y, "", items)
}

// openHiddenPanesMenu lists the panes whose tabs did not fit.
func (s *screen) openHiddenPanesMenu(x, y int) {
	if !s.mainComposerActive() {
		return
	}
	_, hidden := s.tabs(s.width)
	var items []tui.MenuItem
	for _, id := range hidden {
		items = append(items, tui.MenuItem{Label: s.panes.get(id).label, Action: func() { s.activatePane(id) }})
	}
	if len(items) == 0 {
		return
	}
	s.transcriptMenuPane, s.focused().menuItem = s.panes.active, -1
	s.transcriptMenu.Open(x, y, "", items)
}
