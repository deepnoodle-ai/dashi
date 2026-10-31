package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
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
	if terminal, ok := output.(interface{ Fd() uintptr }); ok {
		if width, height, err := term.GetSize(int(terminal.Fd())); err == nil {
			s.width, s.height = width, height
		}
	}
	// Fullscreen turns on the Kitty keyboard protocol where the terminal
	// supports it. Inline asks for it outright, so Shift+Enter inserts a
	// newline in the same terminals; the others ignore the request.
	i.runner = tui.NewInlineApp(tui.WithInlineOutput(output), tui.WithInlineFPS(30), tui.WithInlineBracketedPaste(true),
		tui.WithInlineKittyKeyboard(true))
	s.pendMu.Lock()
	s.send = i.runner.SendEvent
	s.pendMu.Unlock()
	s.handoff = i.runner.Handoff
	s.clipboardOut = output
	s.mouseEnabled = false // Native terminal selection remains available.
	s.nativeScrollback = i.showScrollback
	return i
}

// PaneCommand implements /pane: native scrollback cannot split, so inline
// mode has no panes.
func (*inlineScreen) PaneCommand(string) bool { return false }

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
	i.main().vp.InvalidateAll()
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
	tr := &i.main().tr
	var updates []int
	for id, text := range i.protectionPrinted {
		n, ok := tr.index[id]
		if !ok || n >= i.printed {
			continue
		}
		b := tr.Blocks[n]
		if b.Done && b.Kind == KindCompaction && b.State == "unavailable" && b.Text != text {
			updates = append(updates, n)
		}
	}
	sort.Ints(updates)
	for _, n := range updates {
		b := tr.Blocks[n]
		i.protectionPrinted[b.ID] = b.Text
		i.printCompletedBlock(printable(b))
	}
	for i.printed < len(tr.Blocks) && tr.Blocks[i.printed].Done {
		b := tr.Blocks[i.printed]
		i.printed++ // Print re-renders LiveView synchronously.
		if strings.HasPrefix(b.ID, "active-protection:") {
			i.protectionPrinted[b.ID] = b.Text
		}
		i.printCompletedBlock(printable(b))
	}
}

func (i *inlineScreen) printCompletedBlock(b Block) {
	if text, ok := nativeBlockText(b, i.outputWidth(), !i.noColor); ok {
		i.runner.PrintRaw([]byte(strings.ReplaceAll(text, "\n", "\r\n") + "\r\n\r\n"))
	}
}

func (i *inlineScreen) LiveView() tui.View {
	i.hits.Reset()
	if i.exiting {
		return tui.Text("")
	}
	if i.inspecting {
		view := tui.Text(" Press Enter or Escape to return to Dashi.").Style(hintStyle)
		if i.noColor {
			return withoutColors(view, 1)
		}
		return view
	}
	footer := i.footer()
	_, footerHeight := tui.Measure(footer, i.outputWidth(), 0)
	height := i.height
	if height <= 0 {
		height = 24
	}
	// One row separates the live blocks from the footer (belowTranscript).
	maxLiveHeight := max(1, min(8, height-footerHeight-1))
	liveHeight := 0
	items := inlineItems{i}
	for n := 0; n < items.Len() && liveHeight < maxLiveHeight; n++ {
		_, rows := tui.Measure(items.Item(n), i.outputWidth(), 0)
		if rows > 0 {
			if liveHeight > 0 {
				liveHeight++
			}
			liveHeight += rows
		}
	}
	liveHeight = min(liveHeight, maxLiveHeight)
	// Printed blocks end with an empty row in native scrollback, so the
	// footer alone needs no row above it.
	var view tui.View = footer
	viewHeight := footerHeight
	if liveHeight > 0 {
		view = belowTranscript(tui.Height(liveHeight, tui.Viewport(&i.main().vp, items).Gap(1)), footer)
		viewHeight += liveHeight + 1
	}
	if i.noColor {
		return withoutColors(view, viewHeight)
	}
	return view
}

func (i *inlineScreen) showScrollback(raw bool) {
	i.inspecting = true
	for _, b := range i.main().tr.Blocks {
		b = printable(fullBlock(b))
		if raw {
			i.runner.PrintRaw([]byte(strings.ReplaceAll(FormatBlock(b), "\n", "\r\n") + "\r\n"))
		} else {
			if text, ok := nativeBlockText(b, i.outputWidth(), !i.noColor); ok {
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
	blocks := i.main().tr.Blocks
	for _, b := range blocks[min(i.printed, len(blocks)):] {
		fmt.Fprintln(output, FormatBlock(printable(fullBlock(b))))
	}
}

// Items contain only the live suffix; completed output belongs to scrollback.
type inlineItems struct{ app *inlineScreen }

func (v inlineItems) Len() int {
	n := v.app.main().Len()
	return n - min(v.app.printed, n)
}
func (v inlineItems) Item(n int) tui.View { return v.app.main().Item(v.app.printed + n) }

func (v inlineItems) Source(n int) (string, bool) { return v.app.main().Source(v.app.printed + n) }

// outputWidth uses a default only before a terminal size is available.
func (i *inlineScreen) outputWidth() int {
	if i.width > 0 {
		return i.width
	}
	return 80
}
