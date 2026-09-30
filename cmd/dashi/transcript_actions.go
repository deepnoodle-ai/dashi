package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/clipboard"
	"github.com/deepnoodle-ai/wonton/tui"
)

type inputActionEvent struct {
	at  time.Time
	key tui.KeyEvent
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
	consume := isCtrlC(e) || e.Key == tui.KeyEscape || e.Key == tui.KeyCtrlL ||
		e.Key == tui.KeyPageUp || e.Key == tui.KeyPageDown ||
		(e.Key == tui.KeyHome || e.Key == tui.KeyEnd) && (e.Ctrl || s.input == "")
	if consume {
		s.post(inputActionEvent{at: time.Now(), key: e})
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

func (s *screen) handleTranscriptMouse(e tui.MouseEvent) {
	if !s.mouseEnabled || s.approval != nil || s.asking != nil || s.picker != nil {
		return
	}
	local := e
	local.X--
	inside := local.X >= 0 && local.X < s.vp.Width && local.Y >= 0 && local.Y < s.vp.Height
	if e.Type == tui.MouseScroll && inside {
		if e.Button == tui.MouseButtonWheelUp {
			s.vp.ScrollBy(-1)
		} else if e.Button == tui.MouseButtonWheelDown {
			s.vp.ScrollBy(1)
		}
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
		if ok && line == 0 && i < len(s.tr.Blocks) && s.tr.Blocks[i].Kind == KindTool && s.tr.Blocks[i].Detail != "" {
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
		b.Lines = strings.Split(strings.TrimRight(b.Detail, "\n"), "\n")
	}
	return b
}

func (s *screen) showNativeScrollback(raw bool) {
	if s.runtime == nil {
		s.Emit(warnBlock("Native scrollback is not connected."))
		return
	}
	err := s.runtime.Suspend(func(keys <-chan tui.Event) {
		for _, b := range s.tr.Blocks {
			b = fullBlock(b)
			text := FormatBlock(b)
			if !raw {
				text = tui.Sprint(blockView(b), tui.WithWidth(max(1, s.width)))
			}
			fmt.Fprintln(s.clipboardOut, workspace.PrintableText(text))
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
