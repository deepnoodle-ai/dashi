package main

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deepnoodle-ai/wonton/tui"
)

const dialogStatusInputID = "dialog-status"

// This field temporarily owns input, never the approval or question reply.
// Keep it open after a report so paging and typing cannot answer the dialog.
type dialogStatusInput struct {
	text       string
	prompt     uint64
	generation uint64
}

type dialogStatusKeyEvent struct {
	input *dialogStatusInput
	key   tui.KeyEvent
}

func (dialogStatusKeyEvent) Timestamp() time.Time { return time.Now() }

func (s *screen) beginDialogStatus(text string) {
	s.dialogStatus = &dialogStatusInput{text: text, prompt: s.promptID, generation: s.generation}
}

func (s *screen) dialogStatusActive(p *dialogStatusInput) bool {
	return p != nil && s.dialogStatus == p && s.promptActive && s.promptID == p.prompt && s.generation == p.generation
}

func (s *screen) endDialogStatus() {
	if s.dialogStatus != nil {
		s.ctrl.cancelInspection()
		s.dialogStatus = nil
	}
}

func (s *screen) dialogStatusKey(e tui.KeyEvent) []tui.Cmd {
	switch e.Key {
	case tui.KeyEnter:
		s.dialogStatusSubmit(s.dialogStatus)(s.dialogStatus.text)
	case tui.KeyBackspace:
		_, size := utf8.DecodeLastRuneInString(s.dialogStatus.text)
		if size > 0 {
			s.dialogStatus.text = s.dialogStatus.text[:len(s.dialogStatus.text)-size]
		}
	case tui.KeyEscape:
		s.endDialogStatus()
		return []tui.Cmd{tui.Focus(s.focusID())}
	case tui.KeyPageUp:
		s.focused().vp.PageUp()
	case tui.KeyPageDown:
		s.focused().vp.PageDown()
	case tui.KeyHome:
		if e.Ctrl {
			s.focused().vp.ScrollToTop()
		}
	case tui.KeyEnd:
		if e.Ctrl {
			s.focused().vp.ScrollToBottom()
		}
	default:
		// Keys can arrive before Wonton renders the newly focused field.
		// Retain that prefix here. Once rendered, its editor consumes text.
		if e.Paste != "" {
			s.dialogStatus.text += e.Paste
		} else if e.Rune != 0 && !e.Ctrl && !e.Alt {
			s.dialogStatus.text += string(e.Rune)
		}
	}
	return nil
}

func (s *screen) dialogStatusKeyHook(p *dialogStatusInput) func(tui.KeyEvent) bool {
	return func(e tui.KeyEvent) bool {
		if !s.dialogStatusActive(p) {
			return true
		}
		if isCtrlC(e) {
			return false // Wonton forwards the single interrupt to HandleEvent.
		}
		if isPaneKey(e) {
			return true // A dialog owns the keys; the field must not insert them.
		}
		if e.Key == tui.KeyEscape || e.Key == tui.KeyPageUp || e.Key == tui.KeyPageDown ||
			(e.Key == tui.KeyHome || e.Key == tui.KeyEnd) && e.Ctrl {
			s.post(dialogStatusKeyEvent{input: p, key: e})
			return true
		}
		return false
	}
}

// Open inspection before editing the answer/reason. The bound dialog draft
// stays in its original field; Escape simply returns focus to that field.
func (s *screen) dialogTextKey(id uint64) func(tui.KeyEvent) bool {
	return func(e tui.KeyEvent) bool {
		// A key can reach the old focused field before the inspector renders.
		// Route it to the temporary field instead of submitting/editing the draft.
		if s.promptID == id && s.dialogStatusActive(s.dialogStatus) {
			if isCtrlC(e) {
				return false // native dispatch forwards exactly one interrupt
			}
			s.post(dialogStatusKeyEvent{input: s.dialogStatus, key: e})
			return true
		}
		if e.Key == tui.KeyCtrlS {
			if s.promptActive && s.promptID == id && s.dialogStatus == nil &&
				(s.asking != nil && s.asking.own || s.approval != nil && s.approval.denying) {
				s.beginDialogStatus("/status")
				s.post(focusEvent{id: dialogStatusInputID, prompt: id})
			}
			return true
		}
		return s.inputKey(e)
	}
}

// Paste bypasses OnKey in Wonton. A retained answer/reason field must consume
// it while the inspector owns input, even before its first focused render.
func (s *screen) dialogTextPaste(id uint64) tui.PasteHandler {
	generation, asking, approval := s.generation, s.asking, s.approval
	return func(info tui.PasteInfo) (tui.PasteHandlerDecision, string) {
		if !s.promptActive || s.promptID != id || s.generation != generation || s.asking != asking || s.approval != approval {
			return tui.PasteModified, "" // stale fields cannot edit a replacement prompt
		}
		if s.dialogStatusActive(s.dialogStatus) {
			s.post(dialogStatusKeyEvent{input: s.dialogStatus, key: tui.KeyEvent{Paste: info.Content}})
			return tui.PasteModified, ""
		}
		return tui.PasteAccept, ""
	}
}

func (s *screen) dialogStatusSubmit(p *dialogStatusInput) func(string) {
	return func(value string) {
		if !s.dialogStatusActive(p) {
			return
		}
		if !dialogInspectionCommand(value) {
			s.Emit(warnBlock("Use /status [ID], /tasks, /task ID, or /task-stop ID. Press Escape to return to the pending dialog."))
			return
		}
		p.text = ""
		s.main().vp.ScrollToBottom()
		s.ctrl.Input(value)
	}
}

// Preserve empty-field inspection/task command compatibility. This clears
// only the submitted command. Use Ctrl-S to inspect without editing a nonempty
// dialog draft; user-deleted text is not retained or reconstructed.

func (s *screen) inspectDialogSubmission(value string) bool {
	if !dialogInspectionCommand(value) {
		return false
	}
	if s.asking != nil && s.asking.text == value {
		s.asking.text = ""
	}
	if s.approval != nil && s.approval.reason == value {
		s.approval.reason = ""
	}
	s.beginDialogStatus("")
	s.main().vp.ScrollToBottom()
	s.ctrl.Input(value)
	s.post(focusEvent{id: dialogStatusInputID, prompt: s.promptID})
	return true
}

func (s *screen) dialogStatusView() tui.View {
	p := s.dialogStatus
	heading := "Question still pending"
	if s.approval != nil {
		heading = "Approval still pending · " + s.approval.headline()
	}
	return tui.Stack(
		band(tui.Stack(
			tui.MaxHeight(2, tui.Group(dialogGlyph("?", warningStyle), tui.Text("%s", heading).Wrap().Bold().Flex(1))),
			tui.InputField(&p.text).ID(dialogStatusInputID).Prompt("Inspect:").
				PromptStyle(mutedStyle).
				OnKey(s.dialogStatusKeyHook(p)).OnSubmit(s.dialogStatusSubmit(p)),
		).Gap(0)),
		tui.Text(" /status [ID] · /tasks · /task ID · /task-stop ID · Esc returns").Wrap().Style(hintStyle),
	).Gap(0)
}

// Resolution commands need their own bound confirmation; they cannot share an
// already pending dialog. Exact inspection and selective stop need no answer.
func dialogInspectionCommand(value string) bool {
	name, _ := splitCommand(strings.TrimSpace(value))
	return name == "/status" || name == "/tasks" || name == "/task-stop" || name == "/task"
}
