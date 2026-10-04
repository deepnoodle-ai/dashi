package main

import (
	"strings"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

// nativeBlockText retains full Markdown code lines. A cell-based view clips
// those lines at its frame edge; native terminal scrollback can wrap them.
// The text carries styles as escape codes only when useColor is true.
func nativeBlockText(b Block, width int, useColor bool) (string, bool) {
	b = printable(b)
	view := blockView(b)
	if view == nil {
		return "", false
	}
	if b.Kind != KindAssistant && b.Kind != KindReasoning {
		return tui.Sprint(view, tui.WithWidth(max(1, width)), tui.WithColor(useColor)), true
	}
	md, err := tui.NewMarkdownRenderer().WithMaxWidth(max(1, width-2)).Render(b.Text)
	if err != nil {
		return tui.Sprint(view, tui.WithWidth(max(1, width)), tui.WithColor(useColor)), true
	}
	var out strings.Builder
	if b.Kind == KindReasoning {
		out.WriteString("  Reasoning\n")
	}
	for n, line := range md.Lines {
		if n > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(strings.Repeat(" ", 2+line.Indent))
		for i, segment := range line.Segments {
			text := segment.Text
			if i == len(line.Segments)-1 {
				text = strings.TrimSuffix(text, "\n")
			}
			text = workspace.PrintableText(text)
			if useColor {
				style := segment.Style
				style.URL = ""
				text = style.Apply(text)
			}
			out.WriteString(text)
		}
	}
	return out.String(), true
}
