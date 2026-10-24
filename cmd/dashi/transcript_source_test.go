package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

func TestRuntimeCopySelectionUsesExactSource(t *testing.T) {
	source := "  **bold**\t  世界  \r\nsecond line"
	h := newTranscriptRuntime(t, say(source))
	copied := make(chan string, 1)
	h.scr.rt.SendEvent(transcriptCapture{copy: func(_ context.Context, text string) (clipboardWriteResult, error) {
		copied <- text
		return clipboardWriteResult{notice: "Fixture copied.", confirmed: true}, nil
	}})
	h.app.ctrl.Input("show the source")
	frame := h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "bold") })
	row := transcriptRow(t, frame.text, "bold")
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: 5, Y: row})
	want := "  **bold**\t  世界  "
	h.wait(func(s transcriptState) bool { return s.selected == want })
	h.app.ctrl.Input("/copy")
	select {
	case got := <-copied:
		if got != want {
			t.Fatalf("copy changed source: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("copy did not reach clipboard transport")
	}
	// Escape during a running Turn interrupts it. This assertion is about
	// clearing an idle transcript selection, after its final record finishes.
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle })
	h.key(tui.KeyEvent{Key: tui.KeyEscape})
	h.wait(func(s transcriptState) bool { return s.selected == "" })
	toolSource := "tool row\t  世界  " + strings.Repeat(" x", 50) + " END  \r\nnext\n"
	h.scr.Emit(Block{Kind: KindTool, Name: "read_file", State: StateOK, Summary: "preview", Detail: toolSource, Done: true})
	frame = h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "click to expand") })
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 1, X: 8, Y: transcriptRow(t, frame.text, "click to expand")})
	frame = h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "tool row") && strings.Contains(s.text, "END")
	})
	h.scr.rt.SendEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 3, X: 8, Y: transcriptRow(t, frame.text, "tool row")})
	want, _, _ = strings.Cut(toolSource, "\r\n")
	h.wait(func(s transcriptState) bool { return s.selected == want })
	h.app.ctrl.Input("/copy")
	select {
	case got := <-copied:
		if got != want {
			t.Fatalf("tool copy changed source: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tool copy did not reach clipboard transport")
	}
}
func transcriptRow(t *testing.T, frame, text string) int {
	t.Helper()
	for row, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, text) {
			return row
		}
	}
	t.Fatalf("missing %q in frame:\n%s", text, frame)
	return -1
}
func TestNativeMarkdownRetainsLongCodeAndSanitizesDecodedControls(t *testing.T) {
	code := strings.Repeat("long", 120) + " FINAL-CODE-LINE"
	for _, language := range []string{"", "go"} {
		text, ok := nativeBlockText(Block{Kind: KindAssistant, Text: "```" + language + "\n" + code + "\n```\n&#27;[2J"}, 80, true)
		plain := regexp.MustCompile(`\[[0-9;]*m`).ReplaceAllString(text, "")
		if !ok || !strings.Contains(plain, "FINAL-CODE-LINE") || strings.Contains(text, "\x1b[2J") {
			t.Fatalf("clipped code or executable entity: %q", text)
		}
	}
}
