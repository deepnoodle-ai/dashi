//go:build darwin || linux

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

func TestStatusCancelsBlockedStaleSnapshotWithoutLeaseTakeover(t *testing.T) {
	cfg := inspectionTestConfig(t)
	aSaved := recordedInspectionSession(t, cfg, "current record", false)
	bSaved := recordedInspectionSession(t, cfg, "stale secret preview", false)
	rec, err := cfg.Store.Recorder(bSaved)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(rec.Path(dashi.TurnID(bSaved.ID, 1)), os.O_RDWR, 0)
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
