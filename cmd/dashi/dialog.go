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
func (s *screen) dialogHeight() int {
	height := s.height
	if height == 0 {
		height = 24
	}
	return max(2, min(10, height-9))
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

func (d detailLines) Len() int            { return len(d) }
func (d detailLines) Item(i int) tui.View { return tui.Text(" %s", d[i]).Wrap() }

func approvalDetails(r ApprovalRequest) detailLines {
	lines := detailLines{"Tool: " + r.Tool}
	lines = append(lines, strings.Split("Summary: "+r.Summary, "\n")...)
	if r.Dir != "" {
		lines = append(lines, "Directory: "+r.Dir)
	}
	if r.Command != "" {
		lines = append(lines, strings.Split("Command: "+r.Command, "\n")...)
	}
	if r.Arguments != "" {
		lines = append(lines, "Arguments:")
		lines = append(lines, strings.Split(r.Arguments, "\n")...)
	}
	if r.RememberScope != "" {
		lines = append(lines, "Remembered rule: "+r.RememberScope)
	}
	if r.Preview != "" {
		lines = append(lines, "Preview:")
		lines = append(lines, strings.Split(strings.TrimRight(r.Preview, "\n"), "\n")...)
	}
	return lines
}

func (s *screen) approvalView() tui.View {
	p := s.approval
	views := []tui.View{
		tui.Divider(), tui.MaxHeight(2, tui.Text(" Approval pending · %s", p.req.Summary).Wrap().Fg(tui.ColorYellow).Bold()),
		tui.Height(s.dialogHeight(), tui.Viewport(&p.vp, approvalDetails(p.req)).Gap(0)),
		tui.Text(" ↑/↓ or Page Up/Down inspect · /status [ID] · Esc interrupts").Wrap().Hint(),
	}
	if p.denying {
		views = append(views, tui.Text(" Deny: Enter submits the reason · Ctrl-S inspects without editing.").Hint(),
			tui.InputField(&p.reason).ID(denialInputID).Prompt("Reason:").OnKey(s.dialogTextKey(s.promptID)).OnPaste(s.dialogTextPaste(s.promptID)).OnSubmit(s.denialSubmit(s.promptID)))
	} else {
		views = append(views, tui.Text(" y allow once · a allow for this Dashi run · n deny").Wrap().Bold())
	}
	return tui.Stack(views...).Gap(0)
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
	case 'y', 'Y':
		s.finishApproval(Approval{Allow: true})
	case 'a', 'A':
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
		return tui.Text(" %s", p.q.Prompt).Wrap().Bold()
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
	if p.index == index {
		return tui.Text(" ❯ %s", text).Wrap().Fg(tui.ColorCyan)
	}
	return tui.Text("   %s", text).Wrap()
}

func (s *screen) questionView() tui.View {
	p := s.asking
	views := []tui.View{tui.Divider(), tui.Text(" Question pending").Bold(),
		tui.Height(s.dialogHeight(), tui.Viewport(&p.vp, p).Gap(0))}
	if p.own {
		views = append(views, tui.Text(" Enter submits your words · Ctrl-S inspects · Esc interrupts").Wrap().Hint(),
			tui.InputField(&p.text).ID(questionInputID).Prompt("Answer:").Multiline(true).MaxHeight(3).OnKey(s.dialogTextKey(s.promptID)).OnPaste(s.dialogTextPaste(s.promptID)).OnSubmit(s.questionSubmit(s.promptID)))
	} else {
		views = append(views, tui.Text(" ↑/↓ move · Space selects · Enter chooses action").Wrap().Hint(),
			tui.Text(" Tab submit · o own words · /status [ID] · Esc interrupts").Wrap().Hint())
	}
	if p.err != "" {
		views = append(views, tui.Text(" %s", p.err).Wrap().Fg(tui.ColorYellow))
	}
	return tui.Stack(views...).Gap(0)
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
