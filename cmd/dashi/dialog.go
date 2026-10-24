package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

type focusEvent struct {
	id     string
	prompt uint64
}

func (focusEvent) Timestamp() time.Time { return time.Now() }

const questionInputID = "question-answer"
const denialInputID = "denial-reason"

// The detail viewport is bounded independently of the action rows. Even a
// very long command, diff, or option cannot push the decision off screen.
// It shows ten rows on an 80×24 terminal and grows on a taller one.
func (s *screen) dialogHeight() int {
	height := s.height
	if height == 0 {
		height = 24
	}
	return max(2, min(max(10, height-24), height-10))
}

func (s *screen) focusID() string {
	if s.dialogStatus != nil {
		return dialogStatusInputID
	}
	if s.asking != nil && s.asking.own {
		return questionInputID
	}
	if s.approval != nil && s.approval.denying {
		return denialInputID
	}
	return inputID
}

type detailLines []string

func (d detailLines) Len() int { return len(d) }
func (d detailLines) Item(i int) tui.View {
	return tui.PaddingLTRB(2, 0, 0, 0, tui.Text("%s", d[i]).Wrap())
}

// approvalRows are the approval details. Each row is wrapped already and
// starts two columns into the band, under the title text.
type approvalRows []approvalRow

func (d approvalRows) Len() int { return len(d) }
func (d approvalRows) Item(i int) tui.View {
	r := d[i]
	var text tui.View
	switch r.kind {
	case rowMuted:
		text = tui.Text("%s", r.text).Style(mutedStyle)
	case rowCommand:
		text = tui.Text("%s", r.text).Bold()
	case rowDiff:
		text = diffSourceLineView(r.text, 0)
	default:
		text = tui.Text("%s", r.text)
	}
	return tui.Group(tui.Text("  %s%s", strings.Repeat(" ", r.indent), r.label).Style(mutedStyle), text)
}

// layout returns the details for a screen width columns wide. It is built
// again only when the width or the expansion changes.
func (p *approvalPrompt) layout(width int) approvalLayout {
	if width == 0 {
		width = 80
	}
	width = max(20, width-4)
	remember := p.req.offersRemember()
	if p.laidOut == nil || p.layoutWidth != width || p.layoutRemember != remember {
		if p.laidOut != nil && p.layoutRemember != remember {
			p.vp.InvalidateAll()
		}
		l := layoutApproval(p.req, width, p.expanded)
		p.laidOut, p.layoutWidth, p.layoutRemember = &l, width, remember
	}
	return *p.laidOut
}

// headline names the tool and what the call does, in one line.
func (p *approvalPrompt) headline() string {
	if l := layoutApproval(p.req, 0, false); l.summary != "" {
		return approvalTitle(p.req.Tool) + "  " + l.summary
	}
	return approvalTitle(p.req.Tool)
}

// approvalView shows the title, the details, and the choices on the band.
// The details scroll in a bounded area, so the choices stay on screen.
func (s *screen) approvalView() tui.View {
	p := s.approval
	l := p.layout(s.width)
	title := []tui.View{dialogGlyph("?", warningStyle), tui.Text("%s", approvalTitle(p.req.Tool)).Bold()}
	if l.summary != "" {
		title = append(title, tui.Text("  %s", l.summary))
	}
	views := []tui.View{tui.Group(title...)}
	detailHeight := s.dialogHeight()
	if s.width > 0 && s.width < 72 {
		detailHeight = max(1, detailHeight-3)
	}
	height := min(len(l.rows), detailHeight)
	if height > 0 {
		views = append(views, tui.Height(height, tui.Viewport(&p.vp, approvalRows(l.rows)).Gap(0)))
	}
	if line := p.req.checkLine(); line != "" {
		views = append(views, checkLineView(line, p.req.Check.flagged() || p.req.Hold))
	}
	views = append(views, tui.Text(""))
	if p.denying {
		return tui.Stack(
			band(tui.Stack(append(views,
				tui.InputField(&p.reason).ID(denialInputID).Prompt("Reason:").PromptStyle(mutedStyle).OnKey(s.dialogTextKey(s.promptID)).OnPaste(s.dialogTextPaste(s.promptID)).OnSubmit(s.denialSubmit(s.promptID)))...).Gap(0)),
			tui.Text(" Deny: Enter submits · Ctrl-S inspects").Wrap().Style(hintStyle)).Gap(0)
	}
	var hints []string
	if len(l.rows) > height {
		hints = append(hints, "↑↓ PgUp PgDn scroll")
	}
	if p.expanded {
		hints = append(hints, "e collapses")
	} else if l.clipped {
		hints = append(hints, "e shows all")
	}
	hints = append(hints, "/ commands", "Esc interrupts")
	return tui.Stack(
		band(tui.Stack(append(views, approvalChoices(p.req.offersRemember(), s.width))...).Gap(0)),
		tui.Text(" %s", strings.Join(hints, " · ")).Wrap().Style(hintStyle)).Gap(0)
}

// checkLineView is the decide line of a prompt. A flagged or held command
// stands out; any other answer is quiet.
func checkLineView(line string, warn bool) tui.View {
	t := tui.Text("  %s", line)
	if warn {
		return t.Style(warningStyle).Bold()
	}
	return t.Style(mutedStyle)
}

// approvalChoices are the keys of a prompt. remember offers "allow for this
// run"; a command decide flagged or held offers allow once and deny only.
// Each key letter is in the accent, so the choices need no brackets.
func approvalChoices(remember bool, width int) tui.View {
	keys := []string{"y"}
	labels := []string{"Allow once"}
	if remember {
		keys, labels = append(keys, "a"), append(labels, "Allow for this Dashi run")
	}
	keys, labels = append(keys, "n"), append(labels, "Deny")
	key := func(k string) tui.View { return tui.Text("%s ", k).Style(accentStyle).Bold() }
	if width > 0 && width < 72 {
		// One choice a row; the label wraps on a very narrow terminal.
		rows := make([]tui.View, 0, len(keys))
		for i, k := range keys {
			rows = append(rows, tui.Group(tui.Text("  "), key(k), tui.Text("%s", labels[i]).Wrap().Flex(1)))
		}
		return tui.Stack(rows...).Gap(0)
	}
	choices := make([][]tui.View, len(keys))
	for i, k := range keys {
		choices[i] = []tui.View{key(k), tui.Text("%s", labels[i])}
	}
	views := []tui.View{tui.Text("  ")}
	for i, c := range choices {
		if i > 0 {
			views = append(views, tui.Text("   "))
		}
		views = append(views, c...)
	}
	return tui.Group(views...)
}

func scrollDialog(vp *tui.ViewportState, e tui.KeyEvent) bool {
	switch e.Key {
	case tui.KeyArrowUp:
		vp.ScrollBy(-1)
	case tui.KeyArrowDown:
		vp.ScrollBy(1)
	case tui.KeyPageUp:
		vp.PageUp()
	case tui.KeyPageDown:
		vp.PageDown()
	case tui.KeyHome:
		vp.ScrollToTop()
	case tui.KeyEnd:
		vp.ScrollToBottom()
	default:
		return false
	}
	return true
}

func (s *screen) approvalKey(e tui.KeyEvent) []tui.Cmd {
	p := s.approval
	if e.Key == tui.KeyEscape {
		s.ctrl.CancelWork()
		return nil
	}
	if scrollDialog(&p.vp, e) {
		return nil
	}
	if p.denying {
		return nil
	}
	switch e.Rune {
	case 'e', 'E':
		if p.expanded || p.layout(s.width).clipped {
			p.expanded, p.laidOut = !p.expanded, nil
			p.vp.InvalidateAll()
		}
		return nil
	case 'y', 'Y':
		s.finishApproval(Approval{Allow: true})
	case 'a', 'A':
		if !p.req.offersRemember() {
			return nil
		}
		s.finishApproval(Approval{Allow: true, Remember: true})
	case 'n', 'N':
		p.denying = true
		return []tui.Cmd{tui.Focus(denialInputID)}
	default:
		return nil
	}
	return []tui.Cmd{tui.Focus(inputID)}
}

func (s *screen) finishApproval(a Approval) {
	p := s.approval
	if p == nil {
		return
	}
	if !a.Allow && a.Reason == "" {
		a.Reason = declinedReason
	}
	s.clearPrompts()
	p.reply <- a
	s.post(focusEvent{id: inputID, prompt: s.promptID})
}

// Question item indices retain exact offered labels. The Controller maps
// these indices to the original values after the sanitized display replies.
func (p *askPrompt) Len() int {
	if len(p.q.Choices) == 0 {
		return 1
	}
	return len(p.q.Choices) + 3
}
func (p *askPrompt) Item(i int) tui.View {
	if i == 0 {
		prompt := tui.Text("  %s", p.q.Prompt).Wrap()
		if p.task == "" {
			return prompt
		}
		return tui.Stack(tui.Text("  %s", taskOrigin(p.task)).Wrap().Style(mutedStyle), prompt).Gap(0)
	}
	index := i - 1
	text := ""
	if index < len(p.q.Choices) {
		c := p.q.Choices[index]
		mark := "( )"
		if p.q.Multiple {
			mark = "[ ]"
		}
		if p.selected[index] {
			mark = "[x]"
		}
		text = fmt.Sprintf("%s %d. %s", mark, index+1, c.Label)
		if c.Description != "" {
			text += "\n    " + c.Description
		}
	} else if index == len(p.q.Choices) {
		text = "Submit selected choices"
	} else {
		text = "Answer in my own words"
	}
	return choiceRow(text, p.index == index, true)
}

func (s *screen) questionView() tui.View {
	p := s.asking
	views := []tui.View{tui.Group(dialogGlyph("?", warningStyle), tui.Text("Question pending").Bold()),
		tui.Height(s.dialogHeight(), tui.Viewport(&p.vp, p).Gap(0))}
	var hints []tui.View
	if p.own {
		views = append(views, tui.InputField(&p.text).ID(questionInputID).Prompt("Answer:").PromptStyle(mutedStyle).Multiline(true).MaxHeight(3).OnKey(s.dialogTextKey(s.promptID)).OnPaste(s.dialogTextPaste(s.promptID)).OnSubmit(s.questionSubmit(s.promptID)))
		hints = append(hints, tui.Text(" Enter submits your words · Ctrl-S inspects · Esc interrupts").Wrap().Style(hintStyle))
	} else {
		hints = append(hints, tui.Text(" ↑/↓ move · Space selects · Enter chooses action").Wrap().Style(hintStyle),
			tui.Text(" Tab submit · o own words · / commands · Esc interrupts").Wrap().Style(hintStyle))
	}
	if p.err != "" {
		views = append(views, tui.Text("  %s", p.err).Wrap().Style(warningStyle))
	}
	return tui.Stack(append([]tui.View{band(tui.Stack(views...).Gap(0))}, hints...)...).Gap(0)
}

func (s *screen) questionKey(e tui.KeyEvent) []tui.Cmd {
	p := s.asking
	if e.Key == tui.KeyEscape {
		s.ctrl.CancelWork()
		return nil
	}
	if p.own {
		if e.Key == tui.KeyPageUp || e.Key == tui.KeyPageDown {
			scrollDialog(&p.vp, e)
		}
		return nil
	}
	if p.selected == nil {
		p.selected = map[int]bool{}
	}
	n := len(p.q.Choices)
	previous := p.index
	switch e.Key {
	case tui.KeyArrowUp:
		p.index = max(0, p.index-1)
		p.vp.ScrollToItem(p.index + 1)
	case tui.KeyArrowDown:
		p.index = min(n+1, p.index+1)
		p.vp.ScrollToItem(p.index + 1)
	case tui.KeyPageUp, tui.KeyPageDown, tui.KeyHome, tui.KeyEnd:
		scrollDialog(&p.vp, e)
	case tui.KeyTab:
		p.index = n
		p.vp.ScrollToItem(n + 1)
	case tui.KeyEnter:
		if p.index == n {
			s.submitChoices()
		} else if p.index == n+1 {
			p.own = true
		} else if p.q.Multiple {
			p.selected[p.index] = !p.selected[p.index]
			p.vp.Invalidate(p.index + 1)
		} else {
			s.finishQuestion(questionAnswer{choices: []int{p.index}})
		}
	default:
		if e.Rune == ' ' && p.index < n {
			if !p.q.Multiple {
				p.selected = map[int]bool{}
				p.vp.InvalidateAll()
			}
			p.selected[p.index] = !p.selected[p.index]
			p.vp.Invalidate(p.index + 1)
		}
		if e.Rune == 'o' || e.Rune == 'O' {
			p.own = true
		}
	}
	p.vp.Invalidate(previous + 1)
	p.vp.Invalidate(p.index + 1)
	return []tui.Cmd{tui.Focus(s.focusID())}
}

func (s *screen) submitChoices() {
	p := s.asking
	var indices []int
	for i := range p.q.Choices {
		if p.selected[i] {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		p.err = "Select at least one choice, or answer in your own words."
		return
	}
	if !p.q.Multiple && len(indices) != 1 {
		p.err = "Select one choice."
		return
	}
	s.finishQuestion(questionAnswer{choices: indices})
}
func (s *screen) submitOwnAnswer(value string) {
	if s.asking == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		s.asking.err = "Enter an answer before submitting."
		return
	}
	s.finishQuestion(questionAnswer{text: value})
}
func (s *screen) finishQuestion(a questionAnswer) {
	p := s.asking
	s.clearPrompts()
	p.reply <- a
	s.post(focusEvent{id: inputID, prompt: s.promptID})
}

func (s *screen) denialSubmit(id uint64) func(string) {
	return func(v string) {
		if s.promptActive && s.promptID == id && s.dialogStatus == nil && s.approval != nil && s.approval.denying {
			if s.inspectDialogSubmission(v) {
				return
			}
			s.finishApproval(Approval{Reason: strings.TrimSpace(v)})
		}
	}
}
func (s *screen) questionSubmit(id uint64) func(string) {
	return func(v string) {
		if s.promptActive && s.promptID == id && s.dialogStatus == nil && s.asking != nil && s.asking.own {
			if s.inspectDialogSubmission(v) {
				return
			}
			s.submitOwnAnswer(v)
		}
	}
}
