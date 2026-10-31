package main

import (
	"bytes"
	"fmt"
	"github.com/deepnoodle-ai/wonton/tui"
	"os"
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
	for _, b := range h.scr.main().tr.Blocks {
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

func TestAttachmentOutsideWorkspaceDisclosureBeforeSend(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		for _, hidden := range []bool{false, true} {
			t.Run(fmt.Sprintf("symlink=%t/hidden=%t", symlink, hidden), func(t *testing.T) {
				h := newScreenHarness(t, 0)
				outside := fixtureSelectedText(t, t.TempDir(), strings.Repeat("private-", 20)+".txt", "external selected evidence")
				selected := outside
				if symlink {
					selected = "link.txt"
					if err := os.Symlink(outside, filepath.Join(h.dir, selected)); err != nil {
						t.Fatal(err)
					}
				}
				if hidden {
					for n := range 2 {
						name := fmt.Sprintf("inside-%d.txt", n)
						fixtureSelectedText(t, h.dir, name, "x")
						h.submit("/attach " + name)
					}
					h.waitFor(func() bool {
						return len(h.scr.attachments.entries) == 2 && h.scr.attachments.entries[1].state == attachmentReady
					})
				}
				raw := "inspect selected evidence @" + selected
				h.submit(raw)
				number := 1
				if hidden {
					number = 3
				}
				h.waitFor(func() bool {
					return len(h.scr.attachments.entries) == number && h.scr.attachments.entries[number-1].state == attachmentReady
				})
				if hidden {
					for n := range 12 {
						name := fmt.Sprintf("remaining-%02d.txt", n)
						fixtureSelectedText(t, h.dir, name, "x")
						h.submit("/attach " + name)
					}
					h.waitFor(func() bool {
						return len(h.scr.attachments.entries) == 15 && h.scr.attachments.entries[14].state == attachmentReady
					})
				}
				// Disclosure uses the canonical snapshot metadata. Rendering never
				// resolves or rereads the selected path, even after the source disappears.
				if err := os.Remove(outside); err != nil {
					t.Fatal(err)
				}
				h.mu.Lock()
				h.scr.width, h.scr.height = 80, 24
				if h.scr.input != raw {
					t.Fatal("selection changed the request draft")
				}
				for _, view := range []tui.View{h.scr.View(), newInlineScreen(h.scr, &bytes.Buffer{}).LiveView()} {
					text := tui.SprintScreen(view, tui.WithWidth(80), tui.WithHeight(24)).Text()
					for _, want := range []string{"❯", "outside workspace", "/attachments", "/detach N", "fixture"} {
						mustHave(t, text, want)
					}
					if hidden {
						mustHave(t, text, "13 more selections")
						mustHave(t, text, "1 outside workspace")
					} else {
						mustHave(t, text, "1. ready · outside workspace")
					}
				}
				full := strings.Join(attachmentRows(&h.scr.attachments, h.app.ctrl.attachmentRoot), "\n")
				h.mu.Unlock()
				canonical, err := filepath.EvalSymlinks(filepath.Dir(outside))
				if err != nil {
					t.Fatal(err)
				}
				mustHave(t, full, filepath.Join(canonical, filepath.Base(outside)))
				if h.client.sends != 0 || h.app.eng.Turns(t.Context()) != 0 {
					t.Fatal("outside selection preview started work")
				}
			})
		}
	}
}
