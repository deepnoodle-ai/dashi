package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// The external evidence harness drives this keyless fixture through an actual
// 80x24 PTY. Ordinary unit runs skip it.
func TestAttachmentPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_ATTACHMENT_PTY")
	if mode == "" {
		t.Skip("real terminal evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires a PTY")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, say("Original selected snapshot received."))
	cfg.Dir = os.Getenv("DASHI_ATTACHMENT_WORKSPACE")
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	s.Emit(Block{Kind: KindNotice, Text: "Attachment acceptance: original snapshot, failed selection, detach, send.", Done: true})
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				s.post(attachmentEvidenceEvent{})
			}
		}
	}()
	if mode == "inline" {
		i := &attachmentEvidenceInline{newInlineScreen(s, os.Stdout)}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&attachmentEvidenceScreen{s}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	s.cancelAttachmentSelections()
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	after, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal attributes not restored", err)
	}
}

type attachmentEvidenceEvent struct{}

func (attachmentEvidenceEvent) Timestamp() time.Time { return time.Now() }

type attachmentEvidenceScreen struct{ *screen }

func (s *attachmentEvidenceScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(attachmentEvidenceEvent); ok {
		s.captureAttachmentEvidence()
		return nil
	}
	return s.screen.HandleEvent(e)
}

type attachmentEvidenceInline struct{ *inlineScreen }

func (s *attachmentEvidenceInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(attachmentEvidenceEvent); ok {
		s.captureAttachmentEvidence()
		return nil
	}
	return s.inlineScreen.HandleEvent(e)
}
func (s *screen) captureAttachmentEvidence() {
	path := os.Getenv("DASHI_ATTACHMENT_STATE")
	if path == "" {
		return
	}
	client := s.eng.cfg.Client.(*fakeClient)
	client.mu.Lock()
	sends := client.sends
	var docs []string
	if len(client.inputs) > 0 {
		docs = hydratedDocuments(client.inputs[len(client.inputs)-1])
	}
	client.mu.Unlock()
	state := struct {
		Input     string
		Rows      []string
		Blocks    []Block
		Mode      Mode
		Sends     int
		Documents []string
	}{s.input, attachmentRows(&s.attachments, s.ctrl.attachmentRoot), s.tr.Blocks, s.ctrl.Mode(), sends, docs}
	data, _ := json.Marshal(state)
	temp := filepath.Join(filepath.Dir(path), filepath.Base(path)+".tmp")
	if os.WriteFile(temp, data, 0600) == nil {
		os.Rename(temp, path)
	}
}
