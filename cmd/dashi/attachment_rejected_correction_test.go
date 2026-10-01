package main

import (
	"bytes"
	"fmt"
	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"strings"
	"testing"
)

func TestAttachmentRejectedCorrectionNeverRetriesRemovedEvidence(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			cfg := testConfig(t, say("corrected request received"))
			fixtureSelectedText(t, cfg.Dir, "wrong.txt", "removed evidence")
			fixtureSelectedText(t, cfg.Dir, "right.txt", "correct evidence")
			cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
				return &attachmentAcceptFault{Recorder: r, mode: "rejected"}
			}
			var output bytes.Buffer
			u := newLineUI(&output)
			a, err := buildApp(t.Context(), cfg, u)
			if err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			u.ctrl = a.ctrl
			if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			a.ctrl.Input("/attach wrong.txt")
			a.ctrl.Input("inspect wrong evidence")
			a.ctrl.Wait()
			u.attachmentMu.Lock()
			u.drainAttachmentReceiptsLocked()
			frozen := u.attachments.frozenBy
			u.attachmentMu.Unlock()
			if frozen != "" || a.eng.HasPending() || cfg.Client.(*fakeClient).sends != 0 {
				t.Fatal("rejected request retained ownership or dispatched")
			}
			if _, err := a.eng.Resume(t.Context()); err == nil {
				t.Fatal("rejected request silently resumed")
			}
			a.ctrl.Input("/detach 1")
			if replacement {
				a.ctrl.Input("/attach right.txt")
			}
			a.ctrl.Input("inspect corrected text")
			a.ctrl.Wait()
			client := cfg.Client.(*fakeClient)
			if client.sends != 1 || a.eng.Turns(t.Context()) != 1 || a.eng.HasPending() {
				t.Fatalf("sends=%d turns=%d pending=%v output=%s", client.sends, a.eng.Turns(t.Context()), a.eng.HasPending(), output.String())
			}
			docs := hydratedDocuments(client.inputAt(0))
			if replacement {
				if len(docs) != 1 || docs[0] != "correct evidence" {
					t.Fatalf("documents=%v", docs)
				}
			} else if len(docs) != 0 {
				t.Fatalf("detached evidence reached model: %v", docs)
			}
			text := joinMessages(client.inputAt(0).Messages)
			if !strings.Contains(text, "inspect corrected text") || strings.Contains(text, "wrong evidence") {
				t.Fatalf("wrong request: %s", text)
			}
		})
	}
}

func TestAttachmentRejectedAfterInterruptedSummaryKeepsPendingIdentity(t *testing.T) {
	started := make(chan struct{})
	cfg := testConfig(t, fullWindow, blockUntilDone(started), say("<summary>Retained project facts.</summary>"), say("Retried original evidence."))
	compactWindow(cfg.Client.(*fakeClient))
	var rec *attachmentAcceptFault
	cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
		rec = &attachmentAcceptFault{Recorder: r, mode: "rejected", failed: true}
		return rec
	}
	u := newLineUI(&bytes.Buffer{})
	a, err := buildApp(t.Context(), cfg, u)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	u.ctrl = a.ctrl
	if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.eng.Prompt(t.Context(), compactionSeed()); err != nil {
		t.Fatal(err)
	}
	rec.failed = false
	fixtureSelectedText(t, cfg.Dir, "notes.txt", "summary-held evidence")
	a.ctrl.Input("/attach notes.txt")
	a.ctrl.Input("inspect original")
	<-started
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	original := a.eng.pending
	key := original.start.CommandID
	a.ctrl.Input("")
	a.ctrl.Wait()
	u.attachmentMu.Lock()
	u.drainAttachmentReceiptsLocked()
	frozen := u.attachments.frozenBy
	u.attachmentMu.Unlock()
	if a.eng.pending != original || a.eng.pending.start.CommandID != key || frozen == "" || cfg.Client.(*fakeClient).sends != 3 {
		t.Fatal("ordinary rejection released summary-owned draft or changed identity")
	}
	a.ctrl.Input("/detach 1")
	u.attachmentMu.Lock()
	remaining := len(u.attachments.entries)
	u.attachmentMu.Unlock()
	if remaining != 1 {
		t.Fatal("summary-owned snapshot removed")
	}
	a.ctrl.Input("")
	a.ctrl.Wait()
	u.attachmentMu.Lock()
	u.drainAttachmentReceiptsLocked()
	remaining = len(u.attachments.entries)
	u.attachmentMu.Unlock()
	if a.eng.HasPending() || remaining != 0 || cfg.Client.(*fakeClient).sends != 4 || a.eng.Turns(t.Context()) != 3 {
		t.Fatal("explicit summary retry lost or duplicated input")
	}
	docs := hydratedDocuments(cfg.Client.(*fakeClient).inputAt(3))
	if len(docs) != 1 || docs[0] != "summary-held evidence" {
		t.Fatalf("documents=%v", docs)
	}
}
