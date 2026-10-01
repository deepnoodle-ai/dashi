package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

func TestAttachmentLineBlankConfirmsTextAfterLastDetach(t *testing.T) {
	oldTerminal := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = oldTerminal })
	cfg := testConfig(t, say("remaining text"), say("ordinary text"))
	var stdout, stderr bytes.Buffer
	input := "inspect @missing.txt\n/detach 1\n\nplain prompt\n\n"
	if code := runLines(t.Context(), cfg, Options{Plain: true}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	client := cfg.Client.(*fakeClient)
	if client.sends != 2 {
		t.Fatalf("sends=%d stdout=%s", client.sends, stdout.String())
	}
	if dir := os.Getenv("DASHI_ATTACHMENT_TRANSCRIPTS"); dir != "" {
		proof := "stdin:\n" + input + "\nstdout:\n" + stdout.String() + "\nstderr:\n" + stderr.String() + "\nSend=2; retained inspect request submitted after last detach; next plain prompt unchanged.\n"
		if err := os.WriteFile(filepath.Join(dir, "line-last-detach-confirmation.txt"), []byte(proof), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"inspect ", "plain prompt"} {
		in := client.inputAt(i)
		var text string
		for _, message := range in.Messages {
			if message.Role == "user" {
				for _, part := range message.Content {
					if part.Block != nil && part.Block.Text != nil {
						text = part.Block.Text.Text
					}
				}
			}
		}
		if text != want || len(hydratedDocuments(in)) != 0 {
			t.Fatalf("request %d: text=%q messages=%+v", i, text, in.Messages)
		}
	}
}

func TestAttachmentCommandPathGrammar(t *testing.T) {
	for _, arg := range []string{`notes\ v2.txt`, `"notes v2.txt"`, `'notes v2.txt'`, `notes v2.txt`} {
		t.Run(arg, func(t *testing.T) {
			cfg := testConfig(t, say("selected"))
			fixtureSelectedText(t, cfg.Dir, "notes v2.txt", "selected evidence")
			u := newLineUI(&bytes.Buffer{})
			a, err := buildApp(t.Context(), cfg, u)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.eng.Close)
			u.ctrl = a.ctrl
			if _, err := a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			a.ctrl.Input("/attach " + arg)
			if cfg.Client.(*fakeClient).sends != 0 || len(u.attachments.entries) != 1 || u.attachments.entries[0].state != attachmentReady {
				t.Fatalf("selection started work or failed: %+v", u.attachments.entries)
			}
			a.ctrl.Input("inspect")
			a.ctrl.Wait()
			if docs := hydratedDocuments(cfg.Client.(*fakeClient).inputAt(0)); len(docs) != 1 || docs[0] != "selected evidence" {
				t.Fatalf("documents=%v", docs)
			}
		})
	}
}

func TestAttachmentPrintFlagPathStaysLiteral(t *testing.T) {
	cfg := testConfig(t)
	path := `notes\ v2.txt`
	fixtureSelectedText(t, cfg.Dir, path, "literal backslash evidence")
	fixtureSelectedText(t, cfg.Dir, "notes v2.txt", "wrong decoded path")
	in, err := printAttachmentInput(t.Context(), cfg.Dir, "inspect", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.snapshots) != 1 || string(in.snapshots[0].data) != "literal backslash evidence" {
		t.Fatalf("snapshots=%v", in.snapshots)
	}
}

func TestAttachmentScreenCommandsKeepZeroEntryDraftVisible(t *testing.T) {
	for _, command := range []string{"/attach notes.txt", "/attachments", "/detach invalid", "/detach 1"} {
		t.Run(command, func(t *testing.T) {
			h := newScreenHarness(t, 0, say("selected"))
			fixtureSelectedText(t, h.dir, "notes.txt", "evidence")
			const draft = "retained request"
			h.mu.Lock()
			h.scr.input = draft
			if err := h.scr.attachments.setText(draft); err != nil {
				t.Fatal(err)
			}
			h.mu.Unlock()
			h.submit(command)
			h.app.ctrl.Wait()
			if !h.check(func() bool { return h.scr.input == draft && h.scr.attachments.text == draft }) {
				t.Fatal("command hid or changed the retained request")
			}
			if h.client.sends != 0 {
				t.Fatal("command sent retained request")
			}
			mustHave(t, h.render(), draft)
			h.mu.Lock()
			inline := newInlineScreen(h.scr, &bytes.Buffer{})
			text := tui.SprintScreen(inline.LiveView(), tui.WithWidth(80), tui.WithHeight(30)).Text()
			h.mu.Unlock()
			mustHave(t, text, draft)
		})
	}
}
