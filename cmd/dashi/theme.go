package main

import (
	"time"

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
// The dark palette suits dark terminals. Muted text uses 256-color gray
// 248 (#a8a8a8) without dim: Wonton's Hint and Muted add dim to bright
// black, which nearly disappears on dark themes, and darker grays read
// poorly there too. Faint gray 240 is for decoration only, never for text
// a reader needs. The light palette mirrors it for light terminals. Success,
// warning, and danger stay ANSI colors in both, so they follow the
// terminal's own theme.
var (
	darkPalette = palette{
		muted:    tui.Color(248),
		faint:    tui.Color(240),
		accent:   tui.Color(75), // #5fafff
		code:     tui.Color(236),
		band:     tui.Color(236),
		bandText: tui.Color(252),
		syntax:   "dashi",
	}
	lightPalette = palette{
		muted:    tui.Color(242), // #6c6c6c
		faint:    tui.Color(250), // #bcbcbc
		accent:   tui.Color(26),  // #005fd7
		code:     tui.Color(255), // #eeeeee
		band:     tui.Color(254), // #e4e4e4
		bandText: tui.Color(235), // #262626
		syntax:   "dashi-light",
	}
)

// palette is the colors of one theme.
type palette struct {
	muted, faint, accent tui.Color
	// code is the code background.
	code tui.Color
	// band and bandText are the background and foreground of the band.
	band, bandText tui.Color
	// syntax is the name of the chroma style that code blocks use.
	syntax string
}

// The roles and the styles built from them. useTheme sets them all, so a
// style built from a role belongs in useTheme too: a package-level variable
// initialized from another would keep the dark color.
var (
	colorMuted   tui.Color
	colorFaint   tui.Color
	colorAccent  tui.Color
	colorSuccess = tui.ColorGreen
	colorWarning = tui.ColorYellow
	colorDanger  = tui.ColorRed
	colorCode    tui.Color

	textStyle    = tui.NewStyle()
	mutedStyle   tui.Style
	hintStyle    tui.Style
	faintStyle   tui.Style
	accentStyle  tui.Style
	successStyle = tui.NewStyle().WithForeground(colorSuccess)
	warningStyle = tui.NewStyle().WithForeground(colorWarning)
	dangerStyle  = tui.NewStyle().WithForeground(colorDanger)
	codeStyle    tui.Style

	// The band keeps readable contrast across themes.
	promptBackground tui.Color
	promptStyle      tui.Style

	commandHighlightStyle tui.Style

	// syntaxTheme is the name of the chroma style that code blocks use.
	syntaxTheme string
)

// useTheme sets every role and style to the light palette, or to the dark
// one when light is false. The CLI calls it once at startup, before it
// builds any view.
func useTheme(light bool) {
	p := darkPalette
	if light {
		p = lightPalette
	}
	colorMuted, colorFaint, colorAccent, colorCode = p.muted, p.faint, p.accent, p.code
	mutedStyle = tui.NewStyle().WithForeground(colorMuted)
	hintStyle = mutedStyle
	faintStyle = tui.NewStyle().WithForeground(colorFaint)
	accentStyle = tui.NewStyle().WithForeground(colorAccent)
	codeStyle = tui.NewStyle().WithBackground(colorCode)
	promptBackground = p.band
	promptStyle = tui.NewStyle().WithForeground(p.bandText).WithBackground(promptBackground)
	commandHighlightStyle = accentStyle.WithBold()
	syntaxTheme = p.syntax
}

// The theme setting's values: --theme and DASHI_THEME.
const (
	themeAuto  = "auto"
	themeDark  = "dark"
	themeLight = "light"
)

// detectBackground asks the terminal whether its background is dark or
// light. A variable so tests do not touch a real terminal.
var detectBackground = tui.DetectTerminalBackground

// backgroundTimeout is how long auto waits for the terminal's answer. Most
// terminals answer at once and end the wait; the longer limit gives a slow
// link, such as SSH, time to answer before the screen reads keys.
const backgroundTimeout = 300 * time.Millisecond

// lightTheme reports whether the screen should use the light palette for a
// theme setting; empty means auto. Auto asks the terminal only when the
// screen draws in color, and a terminal that does not say gets the dark
// palette. Call it before anything reads standard input.
func lightTheme(setting string, color bool) bool {
	switch setting {
	case themeLight:
		return true
	case themeDark:
		return false
	}
	if !color {
		return false
	}
	// An error still comes with the best answer available.
	background, _ := detectBackground(backgroundTimeout)
	return background == tui.TerminalBackgroundLight
}

// The syntax colors reuse the accent and one soft string color. Tokens
// without an entry keep the default foreground, and Wonton ignores chroma
// backgrounds, so code shows on the code background.
func init() {
	styles.Register(chroma.MustNewStyle(darkPalette.syntax, chroma.StyleEntries{
		chroma.Keyword:       "#5fafff",
		chroma.KeywordType:   "#5fafff",
		chroma.NameBuiltin:   "#5fafff",
		chroma.LiteralString: "#afd787",
		chroma.LiteralNumber: "#afd787",
		chroma.Comment:       "italic #8a8a8a",
		chroma.Operator:      "#a8a8a8",
		chroma.Punctuation:   "#a8a8a8",
	}))
	styles.Register(chroma.MustNewStyle(lightPalette.syntax, chroma.StyleEntries{
		chroma.Keyword:       "#005fd7",
		chroma.KeywordType:   "#005fd7",
		chroma.NameBuiltin:   "#005fd7",
		chroma.LiteralString: "#3d7a2c",
		chroma.LiteralNumber: "#3d7a2c",
		chroma.Comment:       "italic #767676",
		chroma.Operator:      "#6c6c6c",
		chroma.Punctuation:   "#6c6c6c",
	}))
	useTheme(false)
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
	t.CodeBlockShowLanguage = true
	t.CodeBlockLanguageStyle = mutedStyle
	t.BlockQuoteStyle = mutedStyle.WithItalic()
	t.BlockQuotePrefix = "│ "
	t.BlockQuotePrefixStyle = faintStyle
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
