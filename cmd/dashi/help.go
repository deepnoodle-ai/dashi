package main

import (
	"strings"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// helpDoc is the /help listing: titled sections of commands, skills, or
// keys. A KindHelp block carries it; its Text is the plain layout.
type helpDoc struct {
	Sections []helpSection
}

type helpSection struct {
	Title string
	Rows  []helpRow
	// Note is a one-line remark after the rows, such as a legend.
	Note string
	// Keys aligns its own column instead of the one commands share.
	Keys bool
}

// helpRow is one command, skill, or key. Name is the command with its
// aliases, or the key. During marks a command that works during a turn.
type helpRow struct {
	Name, Usage, Text string
	During            bool
}

const (
	duringMarker = "•"
	helpGutter   = 4
	// A longer command puts its description on the next line.
	helpNameMax = 28
)

func (h helpDoc) printable() helpDoc {
	out := helpDoc{Sections: make([]helpSection, len(h.Sections))}
	for i, s := range h.Sections {
		rows := make([]helpRow, len(s.Rows))
		for j, r := range s.Rows {
			rows[j] = helpRow{workspace.PrintableText(r.Name), workspace.PrintableText(r.Usage), workspace.PrintableText(r.Text), r.During}
		}
		out.Sections[i] = helpSection{workspace.PrintableText(s.Title), rows, workspace.PrintableText(s.Note), s.Keys}
	}
	return out
}

// helpRole names how a span of the help layout is styled.
type helpRole int

const (
	rolePlain helpRole = iota
	roleHeading
	roleMarker
	roleName
	roleUsage
	roleNote
)

type helpSpan struct {
	text string
	role helpRole
}

// lines lays the help out in aligned columns. Joining the span texts of
// each line gives the plain text, so styled spans map back to it exactly.
func (h helpDoc) lines() [][]helpSpan {
	widths := map[bool]int{}
	for _, s := range h.Sections {
		for _, r := range s.Rows {
			if w := runewidth.StringWidth(r.head()); w <= helpNameMax {
				widths[s.Keys] = max(widths[s.Keys], w)
			}
		}
	}
	var out [][]helpSpan
	for i, s := range h.Sections {
		width := widths[s.Keys]
		if i > 0 {
			out = append(out, nil)
		}
		out = append(out, []helpSpan{{s.Title, roleHeading}})
		for _, r := range s.Rows {
			marker := strings.Repeat(" ", helpGutter)
			if r.During {
				marker = "  " + duringMarker + " "
			}
			line := []helpSpan{{marker, roleMarker}, {r.Name, roleName}}
			if r.Usage != "" {
				line = append(line, helpSpan{" " + r.Usage, roleUsage})
			}
			if r.Text == "" {
				out = append(out, line)
				continue
			}
			pad := width - runewidth.StringWidth(r.head())
			if pad < 0 {
				out = append(out, line)
				line, pad = nil, helpGutter+width
			}
			line = append(line, helpSpan{strings.Repeat(" ", pad+2), rolePlain}, helpSpan{r.Text, rolePlain})
			out = append(out, line)
		}
		if s.Note != "" {
			out = append(out, []helpSpan{{s.Note, roleNote}})
		}
	}
	return out
}

func (r helpRow) head() string {
	if r.Usage == "" {
		return r.Name
	}
	return r.Name + " " + r.Usage
}

// String is the plain layout for line mode, -p, and copying.
func (h helpDoc) String() string {
	var out strings.Builder
	for i, line := range h.lines() {
		if i > 0 {
			out.WriteByte('\n')
		}
		for _, span := range line {
			out.WriteString(span.text)
		}
	}
	return out.String()
}

// helpView styles the layout. Each span keeps its offset in the plain text,
// so selection and copying read the same characters.
func helpView(h helpDoc) tui.View {
	lines := h.lines()
	views := make([]tui.View, 0, len(lines))
	offset := 0
	for _, line := range lines {
		if len(line) == 0 {
			views = append(views, tui.Text(" "))
			offset++
			continue
		}
		spans := make([]tui.View, 0, len(line))
		for i, span := range line {
			t := tui.Text("%s", span.text).SourceOffset(offset)
			offset += len(span.text)
			switch span.role {
			case roleHeading:
				t = t.Fg(tui.ColorCyan).Bold()
			case roleMarker, roleUsage:
				t = t.Style(mutedStyle)
			case roleName:
				t = t.Bold()
			case roleNote:
				t = t.Style(hintStyle)
			}
			if i == len(line)-1 {
				t = t.Wrap().Flex(1)
			}
			spans = append(spans, t)
		}
		offset++ // newline
		views = append(views, tui.Group(spans...))
	}
	return tui.Stack(views...).Gap(0)
}
