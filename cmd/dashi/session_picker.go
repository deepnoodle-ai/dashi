package main

import (
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
)

// The selected card has its own scroll area. Long titles and request previews
// cannot push selection controls or the current session identity off an 80x24 UI.
func sessionPickerView(p *pickerState) tui.View {
	title := "Workspace sessions · recorded snapshots"
	hint := " ↑/↓ choose · Page Up/Down inspect · Enter reopen · Esc cancel"
	if p.title != "Workspace sessions" {
		title = p.title + " · recorded child details"
		hint = " Page Up/Down inspect · Enter close · Esc close"
	}
	views := []tui.View{tui.Divider(), tui.Text(" %s", title).Bold()}
	const rows = 4
	start := max(0, min(p.index-rows/2, len(p.options)-rows))
	end := min(len(p.options), start+rows)
	for i := start; i < end; i++ {
		label := p.options[i]
		prefix := "   "
		if i == p.index {
			prefix = " ❯ "
		}
		v := tui.Text("%s%s", prefix, label)
		if i == p.index {
			v = v.Fg(tui.ColorCyan).Bold()
		}
		views = append(views, tui.MaxHeight(1, v))
	}
	if p.index >= 0 && p.index < len(p.details) {
		views = append(views, tui.Divider(), tui.Height(6, tui.Viewport(&p.sessionVP, detailLines(strings.Split(p.details[p.index], "\n"))).Gap(0)))
	}
	views = append(views, tui.Text("%s", hint).Wrap().Hint())
	return tui.Stack(views...).Gap(0)
}
