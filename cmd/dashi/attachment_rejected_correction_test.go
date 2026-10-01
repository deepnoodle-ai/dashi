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
				return &attachmentAdmissionFault{Recorder: r, mode: "rejected"}
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
				t.Fatal("rejected request retained admission ownership or dispatched")
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
