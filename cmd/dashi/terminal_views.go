package main

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/tui"
)

// --- views ---

// View implements tui.Application.
func (s *screen) View() tui.View {
	view := tui.Stack(
		tui.PaddingLTRB(1, 0, 1, 0, tui.Viewport(&s.vp, s).Gap(1)), s.footer(),
	).Gap(0)
	if s.noColor {
		return withoutColors(view, s.height)
	}
	return view
}

func (s *screen) footer() tui.View {
	views := make([]tui.View, 0, 10)
	if hint := s.selectionHint(); hint != "" {
		views = append(views, tui.Text(" %s", hint).Wrap().Style(hintStyle))
	}
	if !s.vp.AtBottom && s.vp.LinesBelow > 0 {
		views = append(views, tui.Text(" %d lines below · Ctrl+End returns to live output", s.vp.LinesBelow).Style(hintStyle))
	}
	switch {
	case s.dialogStatus != nil:
		views = append(views, s.dialogStatusView())
	case s.approval != nil:
		views = append(views, s.approvalView())
	case s.picker != nil:
		views = append(views, pickerView(s.picker))
	case s.asking != nil:
		views = append(views, s.questionView())
	default:
		mode := s.ctrl.Mode()
		if mode == ModeRunning {
			views = append(views, tui.Text(""), s.workingView())
		}
		for _, row := range attachmentPreviewRows(&s.attachments, s.ctrl.attachmentRoot) {
			views = append(views, tui.Text(" %s", row).Style(mutedStyle))
		}
		if hint := s.inputHint(mode); hint != "" {
			views = append(views, tui.Text(" %s", hint).Wrap().Style(hintStyle))
		}
		views = append(views, tui.Text(""), tui.Panel(tui.PaddingLTRB(1, 1, 1, 1, s.inputView(mode))).Style(promptStyle))
	}
	if line := s.decideStatus(); line != "" {
		views = append(views, tui.Text(" %s", line).Fg(tui.ColorYellow))
	}
	views = append(views, s.statusView())
	return tui.Stack(views...).Gap(0)
}

func (s *screen) inputView(mode Mode) tui.View {
	height := s.height
	if height <= 0 {
		height = 24
	}
	maxHeight := max(1, min(8, height/3))
	if len(s.attachments.entries) > 0 {
		maxHeight = max(1, maxHeight-2)
	}
	// The hint line above the field says what input means now; the
	// placeholder stays neutral.
	return tui.InputField(&s.input).
		ID(inputID).
		Prompt("❯").
		PromptStyle(promptStyle.WithBold()).
		TextStyle(promptStyle).
		PlaceholderStyle(mutedStyle.WithBackground(promptBackground)).
		Placeholder("Ask Dashi to do anything").
		History(s.history).
		Multiline(true).
		MaxHeight(maxHeight).
		OnKey(s.inputKey).
		OnPaste(s.inputPaste(s.promptID)).
		OnChange(func(value string) {
			s.exitGuard.reset()
			if s.promptActive {
				return
			}
			if s.attachments.frozenBy != "" {
				s.input = s.attachments.text
				return
			}
			if !strings.HasPrefix(value, "/") {
				s.attachments.setText(value)
			}
			s.updateAttachmentUnsaved()
		}).
		OnComplete(s.completeDraft).
		Highlight(s.highlightCommand).
		OnSubmit(s.inputSubmit(s.promptID))
}

var commandHighlightStyle = tui.NewStyle().WithForeground(tui.ColorCyan).WithBold()

// highlightCommand styles the draft's first word when it names a known
// command, so a typo or an unfinished name stays plain. An answer to a
// question is never a command.
func (s *screen) highlightCommand(value string) []tui.TextRange {
	if s.asking != nil || s.ctrl == nil || !strings.HasPrefix(value, "/") {
		return nil
	}
	end := strings.IndexFunc(value, unicode.IsSpace)
	if end < 0 {
		end = len(value)
	}
	if !s.ctrl.knownCommand(value[:end]) {
		return nil
	}
	return []tui.TextRange{{Start: 0, End: end, Style: commandHighlightStyle}}
}

func (s *screen) inputHint(mode Mode) string {
	switch {
	case s.asking != nil:
		return "Type your answer."
	case mode == ModeCommand && !s.eng.cfg.MCP.Ready():
		return "Starting MCP servers. Submit input after startup finishes; Ctrl-C cancels startup."
	case mode == ModeRunning:
		for i := len(s.tr.Blocks) - 1; i >= 0; i-- {
			b := s.tr.Blocks[i]
			if b.Kind == KindUser {
				break
			}
			if b.Kind == KindCompaction {
				if b.State == StateRunning {
					return "Compacting: new text is queued. Ctrl-C interrupts."
				}
				break
			}
		}
		return "Type to steer. Start with ! to cut the model call."
	case s.eng.HasPending():
		return "Press Enter to continue and deliver the pending prompt."
	}
	// The status line offers /help; the empty row keeps the layout still.
	return ""
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (s *screen) workingView() tui.View {
	frame := spinnerFrames[int(s.frame/3)%len(spinnerFrames)]
	return tui.Group(
		tui.Text(" %s", frame).Style(mutedStyle),
		tui.Text(" Working · Esc interrupts").Style(mutedStyle),
	)
}

// decideStatus is the decide line of the status area, shown only while a
// check is off or paused.
func (s *screen) decideStatus() string {
	if s.ctrl == nil {
		return ""
	}
	return s.ctrl.decide.status()
}

func (s *screen) statusView() tui.View {
	model := workspace.PrintableText(s.model())
	dir := workspace.PrintableText(shortDir(s.dir))
	return tui.CanvasContext(func(ctx *tui.RenderContext) {
		width, _ := ctx.Size()
		help := " " + statusHint + " "
		budget := max(1, width-runewidth.StringWidth(help)-1)
		left := model + " · " + dir
		ctx.PrintTruncated(1, 0, runewidth.Truncate(left, budget, "…"), mutedStyle)
		if width >= 16 {
			ctx.PrintStyled(width-runewidth.StringWidth(help), 0, help, mutedStyle)
		}
	}).Height(1)
}

const statusHint = "/help"

// startupBlock is presentation-only and does not enter recorded Turn history.
func startupBlock(dir string) Block {
	title := "Dashi"
	if version := buildVersion(); version != "" {
		title += " v" + version
	}
	return Block{ID: "cli-header", Kind: KindNotice, Text: title + "\n" + workspace.PrintableText(dir), Done: true}
}

func headerView(b Block) tui.View {
	title, dir, _ := strings.Cut(b.Text, "\n")
	return tui.Stack(
		tui.Group(tui.Text(">_ ").Fg(tui.ColorCyan), tui.Text("%s", title).SourceOffset(0).Bold()),
		tui.PaddingLTRB(3, 0, 0, 0, tui.Text("%s", dir).SourceOffset(len(title)+1).Wrap().Style(mutedStyle)),
	).Gap(0)
}

// shortDir shows the last two parts of a path.
func shortDir(dir string) string {
	parent, base := filepath.Split(filepath.Clean(dir))
	return filepath.Join(filepath.Base(parent), base)
}

func pickerView(p *pickerState) tui.View {
	if p.startupReview != nil {
		return mcpStartupView(p)
	}
	if p.sessionCards {
		return sessionPickerView(p)
	}
	views := []tui.View{tui.Divider(), tui.Text(" %s", p.title).Fg(tui.ColorCyan).Bold()}
	// Keep room for the selected model's facts on an 80×24 terminal.
	window := 10
	if len(p.details) > 0 {
		window = 5
	}
	start := max(0, min(p.index-window/2, len(p.options)-window))
	end := min(len(p.options), start+window)
	for i := start; i < end; i++ {
		if i == p.index {
			views = append(views, tui.Text(" ❯ %s", p.options[i]).Fg(tui.ColorCyan).Bold())
		} else {
			views = append(views, tui.Text("   %s", p.options[i]))
		}
	}
	if p.index >= 0 && p.index < len(p.details) {
		views = append(views, tui.Divider(), tui.Text(" %s", p.options[p.index]).Bold(),
			tui.PaddingLTRB(1, 0, 0, 0, tui.Text("%s", p.details[p.index]).Wrap()))
	}
	views = append(views, tui.Text(" ↑/↓ choose · Enter picks · Esc cancels").Wrap().Style(hintStyle))
	return tui.Stack(views...).Gap(0)
}

// blockView draws one transcript block.
func blockView(b Block) tui.View {
	if b.ID == "cli-header" {
		return headerView(b)
	}
	switch b.Kind {
	case KindUser:
		return tui.Panel(tui.PaddingLTRB(0, 1, 0, 1, tui.Group(
			tui.Text("❯ ").Style(promptStyle).Bold(),
			tui.Text("%s", b.Text).SourceOffset(0).Wrap().Style(promptStyle).Flex(1),
		))).Style(promptStyle)
	case KindAssistant:
		text := b.Text
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return tui.ZStack(
			tui.PaddingLTRB(2, 0, 0, 0, tui.Markdown(text, nil).SourceOffset(0)),
			tui.Text("•").Style(mutedStyle),
		).Align(tui.AlignLeft)
	case KindReasoning:
		if strings.TrimSpace(b.Text) == "" {
			return nil
		}
		return tui.Stack(tui.Text("  Reasoning").Style(mutedStyle), tui.PaddingLTRB(2, 0, 0, 0, tui.Markdown(b.Text, nil).SourceOffset(0))).Gap(0)
	case KindTool:
		return withTaskLine(toolView(b), b)
	case KindTaskStatus, KindTaskResult:
		return taskLineView(b)
	case KindTaskMessage:
		return taskMessageView(b)
	case KindDiff:
		views := make([]tui.View, 0, len(b.Lines))
		offset := 0
		for _, l := range b.Lines {
			views = append(views, tui.PaddingLTRB(2, 0, 0, 0, diffSourceLineView(l, offset)))
			offset += len(l) + 1
		}
		return tui.Stack(views...).Gap(0)
	case KindSteer:
		label := "steer queued"
		switch b.State {
		case SteerDelivered:
			label = "steer delivered"
		case SteerNext:
			label = "queued for the next turn"
		}
		return sourceLabel("  ↳ "+label+": ", b.Text, tui.ColorYellow)
	case KindCompaction:
		color := tui.ColorCyan
		if b.State == StateFailed {
			color = tui.ColorRed
		} else if b.State == "interrupted" {
			color = tui.ColorYellow
		}
		return sourceLabel("  ", b.Text, color)
	case KindPending:
		if b.Done {
			return nil
		}
		return sourceLabel("  ↳ pending prompt: ", b.Text, tui.ColorYellow)
	case KindStop:
		return sourceLabel("■ ", b.Text, tui.ColorYellow)
	case KindFooter:
		return footerView(b, 0)
	case KindHelp:
		if b.Help != nil {
			return helpView(*b.Help)
		}
	case KindWarning:
		return sourceLabel("! ", b.Text, tui.ColorYellow)
	case KindHint:
		return tui.PaddingLTRB(2, 0, 0, 0, tui.Text("%s", b.Text).SourceOffset(0).Wrap().Style(mutedStyle))
	}
	// Notices answer the user, such as /status or /settings, so they use
	// the normal foreground.
	return tui.Text("%s", b.Text).SourceOffset(0).Wrap()
}

// footerSource is the copyable text of a footer at width columns: its
// values and separators, without the leading rule.
func footerSource(b Block, width int) string {
	if b.Footer == nil {
		return b.Text
	}
	parts := b.Footer.fit(width)
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return strings.Join(texts, footerSep)
}

// footerMinRule is the shortest rule a fitted footer keeps after its
// values.
const footerMinRule = 3

// fit returns the footer parts that fit in width columns, leaving out the
// parts a reader needs least first: the model call count, the model, the
// token count, then the cost. The turn number, an incomplete-usage
// warning, and the decide counts stay. Zero width keeps every part.
func (f footerFacts) fit(width int) []footerPart {
	parts := f.parts()
	if width <= 0 {
		return parts
	}
	used := func() int {
		n := runewidth.StringWidth(footerRule) + 1 + 1 + footerMinRule
		for i, p := range parts {
			if i > 0 {
				n += runewidth.StringWidth(footerSep)
			}
			n += runewidth.StringWidth(p.text)
		}
		return n
	}
	for used() > width {
		worst := -1
		for i, p := range parts {
			if p.drop > 0 && (worst < 0 || p.drop > parts[worst].drop) {
				worst = i
			}
		}
		if worst < 0 {
			break
		}
		parts = slices.Delete(parts, worst, worst+1)
	}
	return parts
}

// footerView draws the turn footer as a quiet rule. The values stand out
// a little from the rule and separators; incomplete usage stands out more.
func footerView(b Block, width int) tui.View {
	if b.Footer == nil {
		return tui.Text("%s", b.Text).SourceOffset(0).Style(mutedStyle)
	}
	// The rule is decoration: selection copies the values and separators,
	// which are footerSource.
	views := []tui.View{tui.Text("%s ", footerRule).Style(mutedStyle)}
	offset := 0
	for i, p := range b.Footer.fit(width) {
		if i > 0 {
			views = append(views, tui.Text("%s", footerSep).SourceOffset(offset).Style(mutedStyle))
			offset += len(footerSep)
		}
		t := tui.Text("%s", p.text).SourceOffset(offset)
		offset += len(p.text)
		switch p.kind {
		case footerMeta:
			t = t.Style(mutedStyle)
		case footerCost:
			t = t.Fg(costColor)
		case footerWarn:
			t = t.Fg(tui.ColorYellow)
		}
		views = append(views, t)
	}
	// A flexible stack gives the divider the rest of the row.
	views = append(views, tui.Text(" "), tui.Stack(tui.Divider().Style(mutedStyle)).Flex(1))
	return tui.Group(views...)
}

func toolView(b Block) tui.View {
	var icon *tui.TextView
	var state *tui.TextView
	switch b.State {
	case StateOK:
		icon = tui.Text("•").Fg(tui.ColorGreen)
		state = tui.Text("ok  %.1fs", b.Elapsed.Seconds()).Style(mutedStyle)
	case StateFailed:
		icon = tui.Text("✗").Fg(tui.ColorRed)
		state = tui.Text("failed  %.1fs", b.Elapsed.Seconds()).Fg(tui.ColorRed)
	case StateDenied:
		icon = tui.Text("✗").Fg(tui.ColorYellow)
		state = tui.Text("denied").Fg(tui.ColorYellow)
	case StateWaiting:
		icon = tui.Text("•").Fg(tui.ColorYellow)
		state = tui.Text("waiting").Fg(tui.ColorYellow)
	case StateChecking:
		icon = tui.Text("•").Fg(tui.ColorCyan)
		state = tui.Text("decide: checking command…").Style(mutedStyle)
	case StateCheckingResult:
		icon = tui.Text("•").Fg(tui.ColorCyan)
		state = tui.Text("decide: checking result…").Style(mutedStyle)
	case StateWaitingForYou:
		icon = tui.Text("?").Fg(tui.ColorYellow)
		state = tui.Text("waiting for you").Fg(tui.ColorYellow)
	default:
		icon = tui.Text("•").Fg(tui.ColorCyan)
		state = tui.Text("running").Style(mutedStyle)
	}
	// The name and argument are the tool line's text, so a selection copies
	// them. The icon and the state at the right edge are decoration.
	name, arg := toolHead(b)
	views := []tui.View{icon, tui.Text(" "), tui.Text("%s", name).SourceOffset(0).Bold()}
	if arg != "" {
		views = append(views, tui.Text("%s", toolHeadSep),
			tui.Text("%s", arg).SourceOffset(len(name)+len(toolHeadSep)).Style(mutedStyle).Wrap().Flex(1))
	}
	if arg == "" {
		views = append(views, tui.Spacer())
	}
	views = append(views, tui.Text("  "))
	head := tui.Height(1, tui.Group(append(views, state)...))
	offset := len(toolHeadSource(b)) + 1
	var checks []tui.View
	// Check badges belong with the check details. Keeping them out of the
	// header reserves space for the tool state even on narrow terminals.
	if tick := decideTicks(b); tick != "" {
		checks = append(checks, tui.PaddingLTRB(2, 0, 0, 0,
			tui.Text("%s", tick).Wrap().Style(mutedStyle)))
	}
	for _, mark := range decideLines(b) {
		t := tui.Text("%s", mark.Text).Wrap()
		if mark.Flagged {
			t = t.Fg(tui.ColorYellow)
		} else {
			t = t.Style(mutedStyle)
		}
		checks = append(checks, tui.PaddingLTRB(2, 0, 0, 0, t))
	}
	if b.Expanded && b.Detail != "" {
		views = append([]tui.View{head}, checks...)
		return tui.Stack(append(views, tui.PaddingLTRB(2, 0, 0, 0,
			tui.Text("%s", b.Detail).SourceOffset(offset).Wrap().Style(mutedStyle)))...).Gap(0)
	}
	if len(b.Lines) == 0 && len(checks) == 0 {
		return head
	}
	views = append([]tui.View{head}, checks...)
	for _, l := range b.Lines {
		views = append(views, tui.PaddingLTRB(2, 0, 0, 0,
			tui.Text("%s", l).SourceOffset(offset).Wrap().Style(mutedStyle)))
		offset += len(l) + 1
	}
	return tui.Stack(views...).Gap(0)
}

// toolHeadSep separates a tool's name from its argument on the tool line.
const toolHeadSep = "  "

// toolHead returns the tool name and argument as the tool line shows them.
func toolHead(b Block) (name, arg string) {
	return workspace.PrintableText(b.Name), fitArg(workspace.PrintableText(b.Arg), 70)
}

// toolHeadSource is the copyable text of a tool line's head.
func toolHeadSource(b Block) string {
	name, arg := toolHead(b)
	if arg == "" {
		return name
	}
	return name + toolHeadSep + arg
}

// Secondary text uses 256-color gray 248 (#a8a8a8) without dim. Wonton's
// Hint and Muted add dim to bright black, which nearly disappears on dark
// themes, and darker grays read poorly there too. Footer costs use a soft
// green (#87af87); input bands retain readable contrast across themes.
var (
	mutedStyle       = tui.NewStyle().WithForeground(tui.Color(248))
	hintStyle        = mutedStyle
	costColor        = tui.Color(108)
	promptBackground = tui.Color(236)
	promptStyle      = tui.NewStyle().WithForeground(tui.Color(252)).WithBackground(promptBackground)
)

func sourceLabel(label, source string, color tui.Color) tui.View {
	return tui.Group(tui.Text("%s", label).Fg(color),
		tui.Text("%s", source).SourceOffset(0).Wrap().Fg(color).Flex(1))
}

func diffLineView(indent, line string) tui.View {
	return tui.PaddingLTRB(len(indent), 0, 0, 0, diffSourceLineView(line, 0))
}

func diffSourceLineView(line string, offset int) tui.View {
	t := tui.Text("%s", line).SourceOffset(offset).Wrap()
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return t.Bold()
	case strings.HasPrefix(line, "+"):
		return t.Fg(tui.ColorGreen)
	case strings.HasPrefix(line, "-"):
		return t.Fg(tui.ColorRed)
	case strings.HasPrefix(line, "@@"):
		return t.Fg(tui.ColorCyan)
	}
	return t.Style(mutedStyle)
}

// withoutColors removes color while keeping emphasis, selection, and source mapping.
func withoutColors(view tui.View, height int) tui.View {
	return tui.ZStack(view, tui.CanvasContext(func(ctx *tui.RenderContext) {
		width, height := ctx.Size()
		for y := range height {
			for x := range width {
				style := ctx.Cell(x, y).Style.WithForeground(tui.ColorDefault).WithBackground(tui.ColorDefault)
				ctx.RestyleCell(x, y, style)
			}
		}
	}).Height(height)).Align(tui.AlignLeft)
}
