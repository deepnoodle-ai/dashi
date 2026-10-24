package main

import (
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/deepnoodle-ai/wonton/tui"
)

// The terminal UI draws every color through the roles below. Each role has
// one meaning, so a color tells the reader what kind of thing it marks:
//
//   - text: the default foreground, for anything the user reads.
//   - muted: secondary text such as arguments, metadata, hints, and footer
//     values.
//   - faint: decoration such as rules, borders, separators, and connectors.
//   - accent: what is active or can be acted on: headings, links,
//     selection, running work, the ❯ glyphs, command names, and key hints.
//   - success and danger: finished work that succeeded or failed, and the
//     added and removed lines of a diff.
//   - warning: only things that need the user, such as an approval, a
//     pending question, a limit stop, a flagged check, or a real warning.
//   - code: the background of inline code and code blocks.
//   - band: the background of the prompt and of the user's messages.
//
// Muted text uses 256-color gray 248 (#a8a8a8) without dim. Wonton's Hint
// and Muted add dim to bright black, which nearly disappears on dark
// themes, and darker grays read poorly there too. Faint gray 240 is for
// decoration only, never for text a reader needs.
var (
	colorMuted   = tui.Color(248)
	colorFaint   = tui.Color(240)
	colorAccent  = tui.Color(75) // #5fafff
	colorSuccess = tui.ColorGreen
	colorWarning = tui.ColorYellow
	colorDanger  = tui.ColorRed
	colorCode    = tui.Color(236)

	textStyle    = tui.NewStyle()
	mutedStyle   = tui.NewStyle().WithForeground(colorMuted)
	hintStyle    = mutedStyle
	faintStyle   = tui.NewStyle().WithForeground(colorFaint)
	accentStyle  = tui.NewStyle().WithForeground(colorAccent)
	successStyle = tui.NewStyle().WithForeground(colorSuccess)
	warningStyle = tui.NewStyle().WithForeground(colorWarning)
	dangerStyle  = tui.NewStyle().WithForeground(colorDanger)
	codeStyle    = tui.NewStyle().WithBackground(colorCode)

	// The band keeps readable contrast across themes.
	promptBackground = tui.Color(236)
	promptStyle      = tui.NewStyle().WithForeground(tui.Color(252)).WithBackground(promptBackground)

	commandHighlightStyle = accentStyle.WithBold()
)

// syntaxTheme is the name of the chroma style that code blocks use.
const syntaxTheme = "dashi"

// The syntax colors reuse the accent and one soft string color. Tokens
// without an entry keep the default foreground, and Wonton ignores chroma
// backgrounds, so code shows on the code background.
func init() {
	styles.Register(chroma.MustNewStyle(syntaxTheme, chroma.StyleEntries{
		chroma.Keyword:       "#5fafff",
		chroma.KeywordType:   "#5fafff",
		chroma.NameBuiltin:   "#5fafff",
		chroma.LiteralString: "#afd787",
		chroma.LiteralNumber: "#afd787",
		chroma.Comment:       "italic #8a8a8a",
		chroma.Operator:      "#a8a8a8",
		chroma.Punctuation:   "#a8a8a8",
	}))
}

// markdownTheme styles assistant and reasoning text.
func markdownTheme() tui.MarkdownTheme {
	t := tui.DefaultMarkdownTheme()
	t.H1Style = accentStyle.WithBold()
	t.H2Style = accentStyle.WithBold()
	t.H3Style = textStyle.WithBold()
	t.H4Style = mutedStyle.WithBold()
	t.H5Style = mutedStyle.WithBold()
	t.H6Style = mutedStyle.WithBold()
	t.LinkStyle = accentStyle.WithUnderline()
	t.CodeStyle = codeStyle
	t.CodeBlockStyle = codeStyle
	t.BlockQuoteStyle = mutedStyle.WithItalic()
	t.HorizontalRuleStyle = faintStyle
	t.TableBorderStyle = faintStyle
	t.TableHeaderStyle = textStyle.WithBold()
	t.SyntaxTheme = syntaxTheme
	return t
}

// divider is a faint rule across the width.
func divider() *tui.DividerView {
	return tui.Divider().Style(faintStyle)
}
