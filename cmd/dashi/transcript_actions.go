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
	at           time.Time
	text, notice string
	err          error
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
	// Main Escape must reach native completion before application handling.
	if e.Key == tui.KeyEscape && !s.promptActive {
		return false
	}
	consume := (s.approval != nil && !s.approval.denying) || s.asking != nil && !s.asking.own || s.picker != nil || isCtrlC(e) || e.Key == tui.KeyEscape || e.Key == tui.KeyCtrlL || e.Key == tui.KeyCtrlG ||
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
		text, notice := copyRequest(s.tr.Blocks, s.vp.SelectedText(), arg)
		if notice != "" {
			s.Emit(Block{Kind: KindNotice, Text: notice, Done: true})
			return
		}
		go func() {
			notice, err := s.clipboardWrite(s.ctrl.base, text)
			s.post(clipboardResultEvent{at: time.Now(), text: text, notice: notice, err: err})
		}()
	case "/mouse":
		if arg != "" {
			s.Emit(warnBlock("Usage: /mouse"))
			return
		}
		if s.runtime == nil {
			s.Emit(warnBlock("Native selection is already available in inline mode."))
			return
		}
		s.mouseEnabled = !s.mouseEnabled
		if s.mouseEnabled {
			s.runtime.Terminal().EnableMouseDrag()
			s.Emit(Block{Kind: KindNotice, Text: "Application mouse capture on.", Done: true})
		} else {
			s.vp.ClearSelection()
			s.runtime.Terminal().DisableMouseTracking()
			s.Emit(Block{Kind: KindNotice, Text: "Application mouse capture off. Use native terminal selection.", Done: true})
		}
	case "/scrollback":
		if arg != "" && arg != "raw" {
			s.Emit(warnBlock("Usage: /scrollback [raw]"))
			return
		}
		s.showNativeScrollback(arg == "raw")
	}
}

func (s *screen) clipboardResult(e clipboardResultEvent) {
	if e.err == nil {
		s.Emit(Block{Kind: KindNotice, Text: e.notice, Done: true})
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
	if (s.approval != nil && !s.approval.denying) || s.asking != nil || s.picker != nil {
		// While a dialog is open, the wheel scrolls its details.
		if vp := s.dialogViewport(); vp != nil && e.Type == tui.MouseScroll {
			vp.ScrollBy(wheelDelta(e))
		}
		return
	}
	local := e
	local.X--
	inside := local.X >= 0 && local.X < s.vp.Width && local.Y >= 0 && local.Y < s.vp.Height
	if e.Type == tui.MouseScroll && inside {
		s.vp.ScrollBy(wheelDelta(e))
		return
	}
	if e.Type == tui.MousePress && !inside {
		return
	}
	if s.vp.HandleMouse(local) {
		return
	}
	if e.Type == tui.MouseClick && e.Button == tui.MouseButtonLeft {
		i, line, ok := s.vp.ItemAt(local.X, local.Y)
		if ok && i < len(s.tr.Blocks) && s.tr.Blocks[i].Kind == KindTool && s.tr.Blocks[i].Detail != "" {
			b := s.tr.Blocks[i]
			_, previewHeight := tui.Measure(toolView(b), s.vp.Width, 0)
			if line != 0 && (s.expanded[i] || line < previewHeight) {
				return
			}
			if s.expanded == nil {
				s.expanded = map[int]bool{}
			}
			s.expanded[i] = !s.expanded[i]
			s.vp.Invalidate(i)
		} else if e.Y == s.vp.Height && !s.vp.AtBottom {
			s.vp.ScrollToBottom()
		}
	}
}

func fullBlock(b Block) Block {
	if b.Kind == KindTool && b.Detail != "" {
		b.Expanded = true
		b.Lines = strings.Split(b.Detail, "\n")
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
		for _, b := range s.tr.Blocks {
			b = printable(fullBlock(b))
			text := FormatBlock(b)
			if !raw {
				view := blockView(b)
				if view == nil {
					continue
				}
				text, _ = nativeBlockText(b, max(1, s.width))
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
