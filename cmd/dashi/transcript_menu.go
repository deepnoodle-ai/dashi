package main

import (
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
)

// The transcript menu is the right-click menu on a transcript item in
// fullscreen mode. Its entries depend on the item under the pointer: a tool
// row expands, collapses, and copies its input or output; a message, the
// reasoning, and a diff copy their text. Every copy goes through copyText,
// with the same text a selection of that part would copy.
//
// The menu is modal while open. It takes every key and mouse event, except
// Ctrl+C, which closes it and then does what it always does. It closes when
// the item it acts on changes kind or state, since its entries depend on
// both.

// openTranscriptMenu opens the menu at the screen position (x, y) for the
// item at the viewport position local. It does nothing while a dialog, the
// command menu, or a drag holds the screen, or when the item has no entries.
func (s *screen) openTranscriptMenu(x, y int, local tui.MouseEvent) {
	if !s.mainComposerActive() || s.vp.SelectionActive() || s.commandMenuMatches() != nil {
		return
	}
	var items []tui.MenuItem
	item := -1
	if s.vp.SelectedText() != "" {
		items = append(items, tui.MenuItem{Label: "Copy selection", Hint: "Ctrl+C", Action: func() { s.copySelection() }})
	}
	if i, _, ok := s.vp.ItemAt(local.X, local.Y); ok {
		if more := s.transcriptMenuItems(i); len(more) > 0 {
			item = i
			if len(items) > 0 {
				items = append(items, tui.MenuSeparator())
			}
			items = append(items, more...)
		}
	}
	if len(items) == 0 {
		return
	}
	s.transcriptMenuItem = item
	if item >= 0 {
		s.transcriptMenuShape = shapeOf(s.tr.Blocks[item])
	}
	s.transcriptMenu.Open(x, y, "", items)
}

// transcriptMenuItems returns the menu entries for transcript item i. An
// entry appears only when it has something to act on.
func (s *screen) transcriptMenuItems(i int) []tui.MenuItem {
	if i < 0 || i >= len(s.tr.Blocks) {
		return nil
	}
	b := s.tr.Blocks[i]
	// An entry reads the item's text when chosen, so a message that
	// streamed on after the menu opened is copied whole.
	copyEntry := func(label string, text func() string) []tui.MenuItem {
		if strings.TrimSpace(text()) == "" {
			return nil
		}
		return []tui.MenuItem{{Label: label, Action: func() { s.copyText(text(), false) }}}
	}
	// Source is what a selection of the whole item copies.
	source := func() string {
		text, _ := s.Source(i)
		return text
	}
	switch b.Kind {
	case KindTool:
		var items []tui.MenuItem
		if toolExpandable(b) {
			label := "Expand"
			if s.expanded[i] {
				label = "Collapse"
			}
			items = append(items, tui.MenuItem{Label: label, Action: func() { s.toggleTool(i) }})
		}
		input := func() string {
			text, _ := toolSections(s.tr.Blocks[i])
			return text
		}
		output := func() string {
			_, text := toolSections(s.tr.Blocks[i])
			return text
		}
		items = append(items, copyEntry("Copy input", input)...)
		// A running call's output is only its latest lines.
		if toolEnded(b.State) {
			items = append(items, copyEntry("Copy output", output)...)
		}
		return items
	case KindUser, KindAssistant, KindSteer, KindPending, KindTaskMessage:
		return copyEntry("Copy message", source)
	case KindReasoning:
		return copyEntry("Copy reasoning", source)
	case KindDiff:
		return copyEntry("Copy diff", source)
	}
	return nil
}

// toggleTool expands or collapses tool row i.
func (s *screen) toggleTool(i int) {
	if i < 0 || i >= len(s.tr.Blocks) || s.tr.Blocks[i].Kind != KindTool || !toolExpandable(s.tr.Blocks[i]) {
		return
	}
	if s.expanded == nil {
		s.expanded = map[int]bool{}
	}
	s.expanded[i] = !s.expanded[i]
	s.vp.Invalidate(i)
}

// transcriptMenuKey gives a key to the open menu and reports whether it
// took it. Ctrl+C closes the menu but is not taken, so it still copies a
// selection, interrupts work, or asks to exit.
func (s *screen) transcriptMenuKey(e tui.KeyEvent) bool {
	if !s.transcriptMenu.IsOpen() {
		return false
	}
	if isCtrlC(e) {
		s.transcriptMenu.Close()
		return false
	}
	return s.transcriptMenu.HandleKey(e)
}

// transcriptMenuView draws the open menu on the prompt's band, with the
// chosen entry in the accent color, as in the command menu.
func (s *screen) transcriptMenuView() tui.View {
	selected := commandHighlightStyle
	if s.noColor {
		selected = tui.NewStyle().WithReverse()
	}
	return tui.Menu(&s.transcriptMenu).
		Style(promptStyle).
		BorderStyle(faintStyle).
		SelectedStyle(selected).
		HintStyle(mutedStyle)
}
