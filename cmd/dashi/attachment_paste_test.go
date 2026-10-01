package main

import (
	"fmt"
	"github.com/deepnoodle-ai/wonton/tui"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAttachmentDropRecognition(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{`'/tmp/截图 one.png' /tmp/two\ file.pdf ~/note.txt`, []string{"/tmp/截图 one.png", "/tmp/two file.pdf", "~/note.txt"}},
		{" \nfile:///tmp/one%20two.png\r\nfile://localhost/tmp/spec.pdf\t", []string{"file:///tmp/one%20two.png", "file://localhost/tmp/spec.pdf"}},
		{"use /tmp/file.pdf\nplease", nil}, {"/tmp/file.pdf\nstack: failure", nil}, {"file://remote/tmp/file.pdf", nil}, {"relative.png", nil}, {"'/tmp/incomplete", nil}, {"/tmp/unfinished\\", nil}, {"", nil},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, recognized, overflow := droppedPaths(tc.text)
			if !reflect.DeepEqual(got, tc.want) || recognized != (len(tc.want) > 0) || overflow {
				t.Fatalf("paths=%v recognized=%t overflow=%t", got, recognized, overflow)
			}
		})
	}
	for _, count := range []int{64, 65} {
		paths, yes, over := droppedPaths(strings.Repeat("/missing/file ", count))
		if !yes || over != (count > 64) || len(paths) != min(count, 64) {
			t.Fatal(count, len(paths), yes, over)
		}
	}
	if _, yes, _ := droppedPaths(strings.Repeat("/missing/file ", 65) + "ordinary prose"); yes {
		t.Fatal("overflow prefix swallowed mixed prose")
	}
	atLimit := "/" + strings.Repeat("界", (attachmentPasteBytes-1)/3)
	if len(atLimit) != attachmentPasteBytes {
		t.Fatal(len(atLimit))
	}
	if _, yes, _ := droppedPaths(atLimit); !yes {
		t.Fatal("exact byte boundary refused")
	}
	if _, yes, _ := droppedPaths(atLimit + "x"); yes {
		t.Fatal("oversized segment parsed")
	}
}

func TestAttachmentDropPreflightPreservesDraft(t *testing.T) {
	for _, tc := range []struct {
		name, paste            string
		existing               int
		frozen, active, dialog bool
		decision               tui.PasteHandlerDecision
	}{
		{"too-many", strings.Repeat("/missing/file ", 65), 0, false, false, false, tui.PasteModified},
		{"full", "/missing/file", 64, false, false, false, tui.PasteModified},
		{"partial-overflow", "/missing/one /missing/two", 63, false, false, false, tui.PasteModified},
		{"frozen", "/missing/file", 0, true, false, false, tui.PasteModified},
		{"active", "/missing/file", 0, false, true, false, tui.PasteModified},
		{"dialog", "/missing/file", 0, false, false, true, tui.PasteAccept},
		{"mixed", strings.Repeat("/missing/file ", 65) + "explain this", 0, false, false, false, tui.PasteAccept},
		{"oversized", "/" + strings.Repeat("界", attachmentPasteBytes), 0, false, false, false, tui.PasteAccept},
		{"oversized-prose", strings.Repeat("ordinary log\n", attachmentPasteBytes), 0, false, false, false, tui.PasteAccept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newScreenHarness(t, 0)
			h.mu.Lock()
			h.scr.input = "前 draft\n後"
			h.scr.attachments.setText(h.scr.input)
			for n := range tc.existing {
				h.scr.attachments.reserve(fmt.Sprintf("/existing/%d", n), true)
			}
			if tc.frozen {
				h.scr.attachments.frozenBy = "unresolved"
			}
			if tc.active {
				h.app.ctrl.setMode(ModeRunning)
			}
			h.scr.promptActive = tc.dialog
			before := h.scr.attachments
			decision, text := h.scr.inputPaste(h.scr.promptID)(tui.PasteInfo{Content: tc.paste})
			if decision != tc.decision || text != "" || !reflect.DeepEqual(before, h.scr.attachments) || h.scr.input != "前 draft\n後" || h.scr.attachmentCancel != nil {
				t.Fatal("preflight mutated draft or started selection", decision)
			}
			h.scr.promptActive = false
			h.app.ctrl.setMode(ModeIdle)
			h.mu.Unlock()
			h.app.ctrl.Wait()
			if h.client.sends != 0 || h.app.eng.Turns(t.Context()) != 0 {
				t.Fatal("preflight dispatched")
			}
		})
	}
}

func TestAttachmentDropSnapshotsErrorsAndDetach(t *testing.T) {
	h := newScreenHarness(t, 0, say("received"))
	path := fixtureSelectedText(t, h.dir, "截图 one.txt", "immutable selected evidence")
	urlPath := (&url.URL{Scheme: "file", Path: path}).String()
	h.mu.Lock()
	h.scr.input = "前 explain\n後"
	h.scr.attachments.setText(h.scr.input)
	decision, _ := h.scr.inputPaste(h.scr.promptID)(tui.PasteInfo{Content: urlPath + " /missing/not-there"})
	if decision != tui.PasteModified || len(h.scr.attachments.entries) != 2 || h.scr.attachments.entries[0].state != attachmentLoading || h.scr.attachments.entries[1].state != attachmentLoading {
		t.Fatal("loading entries not reserved synchronously")
	}
	h.scr.submit(h.scr.input)
	h.mu.Unlock()
	h.waitFor(func() bool {
		return h.scr.attachments.entries[0].state == attachmentReady && h.scr.attachments.entries[1].state == attachmentFailed
	})
	if h.client.sends != 0 {
		t.Fatal("paste+Enter bypassed loading")
	}
	h.submit("前 explain\n後")
	if h.client.sends != 0 {
		t.Fatal("error entry dispatched")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.submit("/detach 2")
	if !h.check(func() bool { return h.scr.input == "前 explain\n後" && len(h.scr.attachments.entries) == 1 }) {
		t.Fatal("detach changed prose")
	}
	h.submit("前 explain\n後")
	h.app.ctrl.Wait()
	h.waitFor(func() bool { return len(h.scr.attachments.entries) == 0 })
	if h.client.sends != 1 || h.app.eng.Turns(t.Context()) != 1 || !reflect.DeepEqual(hydratedDocuments(h.client.inputAt(0)), []string{"immutable selected evidence"}) {
		t.Fatal("ready drop did not send once from immutable snapshot")
	}
}

func TestAttachmentDropFocusedRuntimePreservesCursor(t *testing.T) {
	h := newTranscriptRuntime(t, say("selected snapshot answered"))
	path := fixtureSelectedText(t, h.app.eng.cfg.Dir, "截图 space.txt", "native selected bytes")
	const draft = "前 draft\n後"
	h.key(tui.KeyEvent{Paste: draft})
	h.wait(func(s transcriptState) bool { return s.input == draft })
	for range len([]rune(draft)) - 2 {
		h.key(tui.KeyEvent{Key: tui.KeyArrowLeft})
	}
	h.key(tui.KeyEvent{Paste: "'" + path + "'"})
	h.wait(func(s transcriptState) bool { return s.input == draft && strings.Contains(s.text, "1. ready") })
	h.key(tui.KeyEvent{Rune: '✓'})
	const edited = "前 ✓draft\n後"
	h.wait(func(s transcriptState) bool { return s.input == edited })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && s.input == "" && strings.Contains(s.text, "selected snapshot answered")
	})
	h.app.ctrl.Wait()
	client := h.app.eng.cfg.Client.(*fakeClient)
	if client.sends != 1 || !reflect.DeepEqual(hydratedDocuments(client.inputAt(0)), []string{"native selected bytes"}) {
		t.Fatal("focused drop did not send immutable evidence once")
	}
}
