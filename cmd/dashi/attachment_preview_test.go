package main

import (
	"bytes"
	"fmt"
	"github.com/deepnoodle-ai/wonton/tui"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentManySelectionsKeepComposerAndStatusAt80x24(t *testing.T) {
	h := newScreenHarness(t, 0)
	const draft = "visible request draft"
	h.mu.Lock()
	h.scr.width, h.scr.height = 80, 24
	h.scr.input = draft
	h.scr.attachments.setText(draft)
	for n := range 15 {
		path := filepath.Join(h.dir, strings.Repeat("long-directory-", 10), fmt.Sprintf("selected-%02d.txt", n+1))
		draftReady(t, &h.scr.attachments, path, []byte("x"))
	}
	for _, view := range []tui.View{h.scr.View(), newInlineScreen(h.scr, &bytes.Buffer{}).LiveView()} {
		text := tui.SprintScreen(view, tui.WithWidth(80), tui.WithHeight(24)).Text()
		for _, want := range []string{draft, "13 more selections", "/attachments", "/detach N", "fixture"} {
			mustHave(t, text, want)
		}
		if strings.Contains(text, "long-directory-") {
			t.Fatal("footer retained unbounded source paths")
		}
	}
	h.scr.attachmentCommand("/attachments", "")
	h.mu.Unlock()
	h.app.ctrl.Wait()
	h.mu.Lock()
	h.scr.drain()
	var inspection strings.Builder
	for _, b := range h.scr.tr.Blocks {
		inspection.WriteString(b.Text)
	}
	h.mu.Unlock()
	for n := range 15 {
		mustHave(t, inspection.String(), fmt.Sprintf("selected-%02d.txt", n+1))
	}
	mustHave(t, inspection.String(), "long-directory-")
	if h.client.sends != 0 {
		t.Fatal("inspection dispatched")
	}
}
