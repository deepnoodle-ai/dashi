package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
)

// inlineScreen shares controller, prompts, and rendering with fullscreen.
// Only the completed prefix goes to native scrollback. The unfinished suffix
// remains live, so streamed replacements do not print duplicate text.
type inlineScreen struct {
	*screen
	runner            *tui.InlineApp
	printed           int
	generation        uint64
	inspecting        bool
	exiting           bool
	protectionPrinted map[string]string
}

func newInlineScreen(s *screen, output io.Writer) *inlineScreen {
	i := &inlineScreen{screen: s}
	i.runner = tui.NewInlineApp(tui.WithInlineOutput(output), tui.WithInlineFPS(30), tui.WithInlineBracketedPaste(true))
	s.send = i.runner.SendEvent
	s.handoff = i.runner.Handoff
	s.clipboardOut = output
	s.mouseEnabled = false // Native terminal selection remains available.
	s.nativeScrollback = i.showScrollback
	return i
}

func (i *inlineScreen) Init() error {
	i.screen.Init()
	return nil
}

func (i *inlineScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if i.inspecting {
		var key tui.KeyEvent
		switch e := e.(type) {
		case tui.KeyEvent:
			key = e
		case inputActionEvent:
			key = e.key
		}
		if isCtrlC(key) {
			i.ctrl.CancelWork()
		}
		if key.Key == tui.KeyEnter || key.Key == tui.KeyEscape || isCtrlC(key) {
			i.inspecting = false
			return []tui.Cmd{tui.Focus(i.focusID())}
		}
		// Continue receiving work and cancellation while viewing scrollback;
		// pager keystrokes cannot be mistaken for a prompt or permission.
		switch e.(type) {
		case tui.KeyEvent, inputActionEvent, tui.MouseEvent:
			return nil
		}
	}
	cmds := i.screen.HandleEvent(e)
	if i.screen.quitting && !i.exiting {
		i.exiting = true
		i.runner.PrintRaw(nil)
	}
	i.flushCompleted()
	i.vp.InvalidateAll()
	return cmds
}

func (i *inlineScreen) flushCompleted() {
	if i.generation != i.screen.generation {
		i.generation, i.printed = i.screen.generation, 0
		i.protectionPrinted = nil
	}
	if i.protectionPrinted == nil {
		i.protectionPrinted = make(map[string]string)
	}
	// Native scrollback cannot replace a completed row. A recorded warning
	// can replace its initial protection marker after that row was printed,
	// so append the authoritative correction once, before later replies.
	var updates []int
	for id, text := range i.protectionPrinted {
		n, ok := i.tr.index[id]
		if !ok || n >= i.printed {
			continue
		}
		b := i.tr.Blocks[n]
		if b.Done && b.Kind == KindCompaction && b.State == "unavailable" && b.Text != text {
			updates = append(updates, n)
		}
	}
	sort.Ints(updates)
	for _, n := range updates {
		b := i.tr.Blocks[n]
		i.protectionPrinted[b.ID] = b.Text
		i.printCompletedBlock(printable(fullBlock(b)))
	}
	for i.printed < len(i.tr.Blocks) && i.tr.Blocks[i.printed].Done {
		b := i.tr.Blocks[i.printed]
		i.printed++ // Print re-renders LiveView synchronously.
		if strings.HasPrefix(b.ID, "active-protection:") {
			i.protectionPrinted[b.ID] = b.Text
		}
		i.printCompletedBlock(printable(fullBlock(b)))
	}
}

func (i *inlineScreen) printCompletedBlock(b Block) {
	if text, ok := nativeBlockText(b, max(80, i.width), !i.noColor); ok {
		i.runner.PrintRaw([]byte(strings.ReplaceAll(text, "\n", "\r\n") + "\r\n"))
	}
}

func (i *inlineScreen) LiveView() tui.View {
	if i.exiting {
		return tui.Text("")
	}
	if i.inspecting {
		return tui.Text(" Press Enter or Escape to return to Dashi.").Style(hintStyle)
	}
	return tui.Stack(
		tui.Height(8, tui.Viewport(&i.vp, inlineItems{i}).Gap(1)), i.footer(),
	).Gap(0)
}

func (i *inlineScreen) showScrollback(raw bool) {
	i.inspecting = true
	for _, b := range i.tr.Blocks {
		b = printable(fullBlock(b))
		if raw {
			i.runner.PrintRaw([]byte(strings.ReplaceAll(FormatBlock(b), "\n", "\r\n") + "\r\n"))
		} else {
			if text, ok := nativeBlockText(b, max(80, i.width), !i.noColor); ok {
				i.runner.PrintRaw([]byte(strings.ReplaceAll(text, "\n", "\r\n") + "\r\n"))
			}
		}
	}
}

// flushExit writes only the suffix that has not reached native scrollback.
// It runs after the event loop and controller have stopped.
func (i *inlineScreen) flushExit(output io.Writer) {
	i.screen.drain()
	if i.generation != i.screen.generation {
		i.printed = 0
	}
	for _, b := range i.tr.Blocks[min(i.printed, len(i.tr.Blocks)):] {
		fmt.Fprintln(output, FormatBlock(printable(fullBlock(b))))
	}
}

// Items contain only the live suffix; completed output belongs to scrollback.
type inlineItems struct{ app *inlineScreen }

func (v inlineItems) Len() int {
	return len(v.app.tr.Blocks) - min(v.app.printed, len(v.app.tr.Blocks))
}
func (v inlineItems) Item(n int) tui.View { return v.app.screen.Item(v.app.printed + n) }

func (v inlineItems) Source(n int) (string, bool) { return v.app.screen.Source(v.app.printed + n) }
