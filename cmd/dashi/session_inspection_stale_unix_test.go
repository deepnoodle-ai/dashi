//go:build darwin || linux

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	dashisession "github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/wonton/tui"
)

func TestStatusCancelsBlockedStaleSnapshotWithoutLeaseTakeover(t *testing.T) {
	cfg := inspectionTestConfig(t)
	aSaved := recordedInspectionSession(t, cfg, "current record", false)
	bSaved := recordedInspectionSession(t, cfg, "stale secret preview", false)
	rec, err := cfg.Store.Recorder(bSaved)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(rec.Path(dashisession.TurnID(bSaved.ID, 1)), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	ui := newRecorderUI()
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	a.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: aSaved.ID})
	a.ctrl.Input("/status " + bSaved.ID)
	time.Sleep(30 * time.Millisecond)
	a.ctrl.Input("/status " + aSaved.ID)
	a.ctrl.Wait()
	text := ui.text()
	if strings.Contains(text, "stale secret preview") || !strings.Contains(text, "Session "+aSaved.ID+": owned and idle") {
		t.Fatal(text)
	}
	if a.eng.Session().ID != aSaved.ID || cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("status switched or dispatched")
	}
	// Quit must cancel inspection without waiting for the other process's log lock.
	a.ctrl.Input("/status " + bSaved.ID)
	time.Sleep(30 * time.Millisecond)
	a.ctrl.Input("/quit")
	finished := make(chan struct{})
	go func() { a.ctrl.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("quit left a blocked inspection running")
	}

}

func TestNativeStatusReplacementCancelsBlockedScan(t *testing.T) {
	h := newScreenHarness(t, 0, say("Initial answer."))
	h.submit("current request")
	h.app.ctrl.Wait()
	cfg := inspectionTestConfig(t)
	cfg.Dir, cfg.Store = h.dir, h.store
	other := recordedInspectionSession(t, cfg, "stale blocked preview", false)
	rec, err := h.store.Recorder(other)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(rec.Path(dashisession.TurnID(other.ID, 1)), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	h.mu.Lock()
	pending := &approvalPrompt{reply: make(chan Approval, 1)}
	h.scr.HandleEvent(promptEvent{id: 1, approval: pending})
	h.scr.HandleEvent(tui.KeyEvent{Rune: '/'})
	old := h.scr.dialogStatus
	h.scr.dialogStatusSubmit(old)("/status " + other.ID)
	h.mu.Unlock()
	h.waitFor(func() bool { return strings.Contains(dialogTranscriptText(h.scr), "Loading recorded status") })
	h.mu.Lock()
	replacement := &askPrompt{reply: make(chan questionAnswer, 1), own: true, text: "new draft"}
	h.scr.HandleEvent(promptEvent{id: 2, asking: replacement})
	h.scr.dialogStatusSubmit(old)("/status " + other.ID)
	h.scr.HandleEvent(dialogStatusKeyEvent{input: old, key: tui.KeyEvent{Key: tui.KeyEscape}})
	h.mu.Unlock()
	finished := make(chan struct{})
	go func() { h.app.ctrl.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("replaced native prompt retained a blocked inspection")
	}
	if !h.check(func() bool {
		return h.scr.asking == replacement && replacement.text == "new draft" && len(replacement.reply) == 0 && len(pending.reply) == 0 && !strings.Contains(dialogTranscriptText(h.scr), "stale blocked preview")
	}) {
		t.Fatal("stale scan or input affected the replacement question")
	}
}
