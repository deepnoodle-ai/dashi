package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/wonton/clipboard"
	"github.com/deepnoodle-ai/wonton/tui"
)

type inputActionEvent struct {
	at     time.Time
	key    tui.KeyEvent
	prompt uint64
}

func (e inputActionEvent) Timestamp() time.Time { return e.at }

type transcriptCommandEvent struct {
	at        time.Time
	name, arg string
}

func (e transcriptCommandEvent) Timestamp() time.Time { return e.at }

type clipboardResultEvent struct {
	at                time.Time
	text              string
	result            clipboardWriteResult
	err               error
	selectionRevision uint64
	generation        uint64
	// pane is the ID of the pane whose selection was copied, or empty
	// when the text was not a selection.
	pane string
}

func (e clipboardResultEvent) Timestamp() time.Time { return e.at }

func (s *screen) TranscriptCommand(name, arg string) {
	s.post(transcriptCommandEvent{at: time.Now(), name: name, arg: arg})
}

func (s *screen) inputKey(e tui.KeyEvent) bool {
	// Wonton always forwards Ctrl-C to the app, even if the field consumes it.
	// Posting a second event would interrupt twice and could exit the process.
	if isCtrlC(e) {
		return false
	}
	s.exitGuard.reset()
	if s.transcriptMenuKey(e) {
		return true
	}
	if s.commandMenuKey(e) {
		return true
	}
	// Main Escape must reach native completion before application handling.
	if e.Key == tui.KeyEscape && !s.promptActive {
		return false
	}
	consume := (s.approval != nil && !s.approval.denying) || s.asking != nil && !s.asking.own || s.picker != nil || isCtrlC(e) || e.Key == tui.KeyEscape || e.Key == tui.KeyCtrlL || e.Key == tui.KeyCtrlG || e.Key == tui.KeyF2 ||
		e.Key == tui.KeyPageUp || e.Key == tui.KeyPageDown ||
		(e.Key == tui.KeyHome || e.Key == tui.KeyEnd) && (e.Ctrl || s.input == "")
	if consume {
		id := uint64(0)
		if s.promptActive {
			id = s.promptID
		}
		s.post(inputActionEvent{at: time.Now(), key: e, prompt: id})
	}
	return consume
}

func (s *screen) transcriptCommand(name, arg string) {
	s.drain()
	switch name {
	case "/copy":
		// Without a selection, code blocks come from the conversation.
		text, notice := copyRequest(s.main().tr.Blocks, s.selectedText(), arg)
		if notice != "" {
			s.Emit(Block{Kind: KindNotice, Text: notice, Done: true})
			return
		}
		var from *transcriptPane
		if strings.TrimSpace(arg) == "" {
			from = s.selectionPane()
		}
		s.copyText(text, from)
	case "/mouse":
		if arg != "" {
			s.Emit(warnBlock("Usage: /mouse"))
			return
		}
		s.toggleMouse()
	case "/scrollback":
		if arg != "" && arg != "raw" {
			s.Emit(warnBlock("Usage: /scrollback [raw]"))
			return
		}
		s.showNativeScrollback(arg == "raw")
	}
}

// Main-composer shortcuts never take precedence over a pending decision.
func (s *screen) mainComposerActive() bool {
	return !s.promptActive && s.approval == nil && s.asking == nil && s.picker == nil && s.dialogStatus == nil
}

// selectionPane returns the pane that holds a selection, if any.
func (s *screen) selectionPane() *transcriptPane {
	for _, p := range s.paneList() {
		if p.vp.HasSelection() {
			return p
		}
	}
	return nil
}

// selectedText is the text of the selection, if any.
func (s *screen) selectedText() string {
	if p := s.selectionPane(); p != nil {
		return p.vp.SelectedText()
	}
	return ""
}

func (s *screen) selectionHint() string {
	// A selection over only a running tool row has no text, so Ctrl+C
	// would interrupt; offer copy only when there is text to copy.
	if !s.mainComposerActive() || s.selectedText() == "" {
		return ""
	}
	if s.ctrl.Mode() == ModeRunning || s.ctrl.Mode() == ModeApproval {
		return "Ctrl+C copies selection · Esc stops work · F2 native selection"
	}
	return "Ctrl+C copies selection · Esc clears · F2 native selection"
}

func (s *screen) copySelection() bool {
	text := s.selectedText()
	if text == "" {
		return false
	}
	s.copyText(text, s.selectionPane())
	return true
}

// copyText writes text to the clipboard. When text is the selection of
// pane from, a confirmed write clears that selection; from is nil
// otherwise.
func (s *screen) copyText(text string, from *transcriptPane) {
	event := clipboardResultEvent{text: text, generation: s.generation}
	if from != nil {
		event.pane, event.selectionRevision = from.id, from.selectionRevision
	}
	write := s.clipboardWrite
	go func() {
		event.result, event.err = write(s.ctrl.base, text)
		event.at = time.Now()
		s.post(event)
	}()
}

func (s *screen) toggleMouse() {
	if s.runtime == nil {
		s.Emit(Block{Kind: KindNotice, Text: "Native selection is already available in inline mode.", Done: true})
		return
	}
	s.mouseEnabled = !s.mouseEnabled
	if s.mouseEnabled {
		s.runtime.Terminal().EnableMouseDrag()
		s.Emit(Block{Kind: KindNotice, Text: "Application mouse capture on. Drag to select; Ctrl+C copies. F2 enables native selection.", Done: true})
	} else {
		for _, p := range s.paneList() {
			p.clearSelection()
		}
		s.transcriptMenu.Close()
		s.runtime.Terminal().DisableMouseTracking()
		s.Emit(Block{Kind: KindNotice, Text: "Application mouse capture off. Use native terminal selection and copying; F2 restores capture.", Done: true})
	}
}

func (s *screen) clipboardResult(e clipboardResultEvent) {
	if e.err == nil {
		if p := s.panes[e.pane]; p != nil && e.result.confirmed && e.generation == s.generation && e.selectionRevision == p.selectionRevision && p.vp.SelectedText() == e.text {
			p.vp.ClearSelection()
		}
		s.Emit(Block{Kind: KindNotice, Text: e.result.notice, Done: true})
		return
	}
	if s.clipboardOut == nil {
		s.Emit(warnBlock("Clipboard unavailable. Use /scrollback for native terminal copying."))
		return
	}
	if err := clipboard.WriteOSC52(s.clipboardOut, e.text); err != nil {
		s.Emit(warnBlock("Clipboard request failed: %v. Use /scrollback for native terminal copying.", err))
		return
	}
	s.Emit(Block{Kind: KindNotice, Text: "Sent a terminal clipboard request (OSC52). Receipt is unconfirmed.", Done: true})
}

// wheelStep is how many rows one wheel notch scrolls, in the transcript and
// in dialog details alike. Wonton leaves the wheel to the application.
const wheelStep = 3

func wheelDelta(e tui.MouseEvent) int {
	switch e.Button {
	case tui.MouseButtonWheelUp:
		return -wheelStep
	case tui.MouseButtonWheelDown:
		return wheelStep
	}
	return 0
}

// dialogViewport returns the scrollable details of the open dialog, if any.
func (s *screen) dialogViewport() *tui.ViewportState {
	switch {
	case s.approval != nil:
		return &s.approval.vp
	case s.asking != nil:
		return &s.asking.vp
	case s.picker != nil && s.picker.startupReview != nil:
		return &s.picker.startupVP
	case s.picker != nil && s.picker.sessionCards:
		return &s.picker.sessionVP
	}
	return nil
}

func (s *screen) handleTranscriptMouse(e tui.MouseEvent) {
	if !s.mouseEnabled {
		return
	}
	// An open transcript menu takes every mouse event. A press outside it
	// closes it, and nothing else sees that press.
	if s.transcriptMenu.HandleMouse(e) {
		return
	}
	if (s.approval != nil && !s.approval.denying) || s.asking != nil || s.picker != nil {
		// While a dialog is open, the wheel scrolls its details.
		if vp := s.dialogViewport(); vp != nil && e.Type == tui.MouseScroll {
			vp.ScrollBy(wheelDelta(e))
		}
		return
	}
	// Dragging a gutter between panes takes the pointer.
	if s.split.HandleMouse(e) {
		return
	}
	p := s.mousePane(e)
	local, inside := p.local(e)
	if e.Type == tui.MouseScroll && inside {
		p.vp.ScrollBy(wheelDelta(e))
		return
	}
	if e.Type == tui.MousePress && !inside {
		return
	}
	if e.Button == tui.MouseButtonRight {
		if e.Type == tui.MouseClick && inside {
			s.openTranscriptMenu(p, e.X, e.Y, local)
		}
		return
	}
	if e.Button == tui.MouseButtonLeft && (e.Type == tui.MousePress || e.Type == tui.MouseClick || e.Type == tui.MouseDrag || e.Type == tui.MouseMove) {
		p.selectionRevision++
	}
	if p.vp.HandleMouse(local) {
		return
	}
	if e.Type == tui.MouseClick && e.Button == tui.MouseButtonLeft && !p.clickTool(local.X, local.Y) {
		if area, ok := s.hits.At(e.X, e.Y); ok && area == linesBelowArea {
			s.focused().vp.ScrollToBottom()
		}
	}
}

// mousePane is the pane a transcript mouse event goes to. A pane whose
// selection is being dragged keeps the pointer until the drag ends, even
// outside it; otherwise it is the pane under the pointer, or the focused
// pane when there is none, as below the transcript.
func (s *screen) mousePane(e tui.MouseEvent) *transcriptPane {
	for _, p := range s.paneList() {
		if p.vp.SelectionActive() {
			return p
		}
	}
	if p := s.paneAt(e.X, e.Y); p != nil {
		return p
	}
	return s.focused()
}

// fullBlock is b with a tool row expanded, for the views that cannot take
// a click, such as /scrollback and the transcript printed at exit.
func fullBlock(b Block) Block {
	if b.Kind == KindTool && toolExpandable(b) {
		b.Expanded = true
	}
	return b
}

func (s *screen) showNativeScrollback(raw bool) {
	if s.nativeScrollback != nil {
		s.nativeScrollback(raw)
		return
	}
	if s.runtime == nil {
		s.Emit(warnBlock("Native scrollback is not connected."))
		return
	}
	err := s.runtime.Suspend(func(keys <-chan tui.Event) {
		for _, b := range s.main().tr.Blocks {
			b = printable(fullBlock(b))
			text := FormatBlock(b)
			if !raw {
				view := blockView(b)
				if view == nil {
					continue
				}
				text, _ = nativeBlockText(b, max(1, s.width), !s.noColor)
			}
			fmt.Fprintln(s.clipboardOut, text)
		}
		fmt.Fprintln(s.clipboardOut, "Press Enter or Escape to return.")
		for {
			select {
			case <-s.ctrl.base.Done():
				return
			case ev := <-keys:
				if key, ok := ev.(tui.KeyEvent); ok && (key.Key == tui.KeyEnter || key.Key == tui.KeyEscape) {
					return
				}
			}
		}
	})
	if err != nil {
		s.Emit(warnBlock("Terminal restore failed: %v", err))
		s.quitting = true
		s.post(quitEvent{at: time.Now()})
	}
}
