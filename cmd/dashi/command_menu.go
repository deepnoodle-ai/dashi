package main

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// commandMenuItem is one command the command menu offers.
type commandMenuItem struct {
	name        string
	aliases     []string
	usage       string
	description string
}

// runsAlone reports whether the command can run with no arguments: its
// usage is empty or names only optional arguments.
func (item commandMenuItem) runsAlone() bool {
	return item.usage == "" || strings.HasPrefix(item.usage, "[")
}

// commandMenuRows is the most commands the menu shows at once.
const commandMenuRows = 5

// commandMenuNameWidth caps the name column, so one long skill name does
// not push every description to the right.
const commandMenuNameWidth = 24

// commandMenu is the state of the menu of slash commands shown under the
// composer while the user types a command name. The matches themselves are
// computed from the draft each time.
type commandMenu struct {
	// typing is true after a key that edits the draft. Esc, Tab, Enter, and
	// history recall clear it, so the menu stays closed until the next edit.
	typing bool
	// draft is the draft that selected and offset belong to.
	draft    string
	selected int
	// offset is the index of the first command shown.
	offset int
	// complete is the command the next Tab completion inserts.
	complete string
	// submit is the command the next submission runs in place of the draft.
	submit string
}

// noteKey records whether a key the menu did not use edits the draft.
func (m *commandMenu) noteKey(e tui.KeyEvent) {
	switch e.Key {
	case tui.KeyEscape, tui.KeyTab, tui.KeyEnter, tui.KeyArrowUp, tui.KeyArrowDown:
		m.typing = false
	case tui.KeyArrowLeft, tui.KeyArrowRight, tui.KeyHome, tui.KeyEnd, tui.KeyPageUp, tui.KeyPageDown,
		tui.KeyCtrlL, tui.KeyCtrlG, tui.KeyF2:
	default:
		m.typing = true
	}
}

// matchCommandMenu returns the commands whose name or alias matches the
// typed name, in completion order: exact, prefix, then subsequence matches,
// each by name. A command appears once, under its own name.
func matchCommandMenu(typed string, items []commandMenuItem) []commandMenuItem {
	type ranked struct {
		item commandMenuItem
		rank int
	}
	var found []ranked
	for _, item := range items {
		rank := completionRank(typed, item.name)
		for _, alias := range item.aliases {
			rank = min(rank, completionRank(typed, alias))
		}
		if rank < 3 {
			found = append(found, ranked{item, rank})
		}
	}
	slices.SortStableFunc(found, func(a, b ranked) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.item.name, b.item.name)
	})
	out := make([]commandMenuItem, len(found))
	for i, f := range found {
		out[i] = f.item
	}
	return out
}

// commandMenuOffset returns the first shown index that keeps selected in
// view among n commands, moving offset as little as possible.
func commandMenuOffset(selected, offset, n int) int {
	if selected < offset {
		offset = selected
	}
	if selected >= offset+commandMenuRows {
		offset = selected - commandMenuRows + 1
	}
	return max(0, min(offset, n-commandMenuRows))
}

// commandMenuMatches returns the commands the menu shows for the draft, or
// nil when the menu is closed. The menu is open while the main composer's
// draft is a slash and an unfinished command name, after an edit.
func (s *screen) commandMenuMatches() []commandMenuItem {
	m := &s.commandMenu
	if s.input != m.draft {
		m.draft, m.selected, m.offset = s.input, 0, 0
	}
	if !m.typing || s.ctrl == nil || !s.mainComposerActive() || !s.eng.cfg.MCP.Ready() ||
		!strings.HasPrefix(s.input, "/") || strings.IndexFunc(s.input, unicode.IsSpace) >= 0 {
		return nil
	}
	matches := matchCommandMenu(s.input, s.ctrl.commandMenuItems())
	if len(matches) == 0 {
		return nil
	}
	m.selected = min(m.selected, len(matches)-1)
	m.offset = commandMenuOffset(m.selected, m.offset, len(matches))
	return matches
}

// commandMenuKey handles a composer key while the menu is open and reports
// whether it consumed the key. Tab and Enter return false where the input
// field must apply the change: the field asks completeDraft for the Tab
// completion and calls inputSubmit for Enter.
func (s *screen) commandMenuKey(e tui.KeyEvent) bool {
	m := &s.commandMenu
	// The field uses these during the key that set them; never later.
	m.complete, m.submit = "", ""
	matches := s.commandMenuMatches()
	if matches == nil {
		m.noteKey(e)
		return false
	}
	switch {
	case e.Key == tui.KeyArrowUp:
		m.selected = (m.selected - 1 + len(matches)) % len(matches)
	case e.Key == tui.KeyArrowDown:
		m.selected = (m.selected + 1) % len(matches)
	case e.Key == tui.KeyEscape:
		m.typing = false
	case e.Key == tui.KeyTab && !e.Shift:
		m.typing = false
		m.complete = matches[m.selected].name
		return false
	case e.Key == tui.KeyEnter && !e.Shift:
		item := matches[m.selected]
		m.typing = false
		if !item.runsAlone() && !strings.EqualFold(s.input, item.name) {
			s.input = item.name + " "
			return true
		}
		m.submit = item.name
		return false
	default:
		m.noteKey(e)
		return false
	}
	m.offset = commandMenuOffset(m.selected, m.offset, len(matches))
	return true
}

// commandMenuView draws the open menu: up to commandMenuRows commands, a
// count of those above and below, and the keys.
func (s *screen) commandMenuView(matches []commandMenuItem) tui.View {
	m := s.commandMenu
	nameWidth := 0
	for _, item := range matches {
		nameWidth = max(nameWidth, runewidth.StringWidth(item.name))
	}
	nameWidth = min(nameWidth, commandMenuNameWidth)
	views := make([]tui.View, 0, commandMenuRows+5)
	if m.offset > 0 {
		views = append(views, tui.Text("   ↑ %d more", m.offset).Style(mutedStyle))
	}
	end := min(len(matches), m.offset+commandMenuRows)
	for i := m.offset; i < end; i++ {
		views = append(views, commandMenuRow(matches[i], nameWidth, i == m.selected))
	}
	if below := len(matches) - end; below > 0 {
		views = append(views, tui.Text("   ↓ %d more", below).Style(mutedStyle))
	}
	keyStyle := tui.NewStyle().WithBold()
	hint := tui.Group(
		tui.Text("   "),
		tui.Text("↑/↓").Style(keyStyle), tui.Text(" Navigate · ").Style(mutedStyle),
		tui.Text("enter").Style(keyStyle), tui.Text(" Select · ").Style(mutedStyle),
		tui.Text("tab").Style(keyStyle), tui.Text(" Complete").Style(mutedStyle),
		tui.Spacer(),
	)
	views = append(views, tui.Text(""), tui.Height(1, hint))
	return tui.Stack(views...).Gap(0)
}

// commandMenuRow draws one command on one row, cut to the width.
func commandMenuRow(item commandMenuItem, nameWidth int, selected bool) tui.View {
	return tui.CanvasContext(func(ctx *tui.RenderContext) {
		width, _ := ctx.Size()
		marker, nameStyle := "  ", tui.NewStyle()
		if selected {
			marker, nameStyle = "> ", commandHighlightStyle
		}
		x := 1
		ctx.PrintTruncated(x, 0, marker, nameStyle)
		x += 2
		name := runewidth.Truncate(item.name, max(0, width-x), "…")
		ctx.PrintTruncated(x, 0, name, nameStyle)
		x += max(nameWidth, runewidth.StringWidth(name)) + 2
		if x < width && item.description != "" {
			ctx.PrintTruncated(x, 0, runewidth.Truncate(capitalize(item.description), width-x, "…"), mutedStyle)
		}
	}).Height(1)
}

// capitalize upper-cases the first letter of text.
func capitalize(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return string(unicode.ToUpper(r)) + text[size:]
}
