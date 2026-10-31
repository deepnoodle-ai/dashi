package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// Drives the production Wonton/controller stack through a real 80x24 PTY.
func TestSessionInspectionPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_SESSION_PTY")
	if mode == "" {
		t.Skip("real terminal evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires PTY")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := inspectionTestConfig(t, say("Current investigation answer."), say("Follow-up answer."))
	cfg.Dir = os.Getenv("DASHI_SESSION_WORKSPACE")
	other := recordedInspectionSession(t, cfg, "retry handler investigation", true)
	bad, _ := cfg.Store.Create(cfg.Dir, cfg.Model)
	bad.Title = "damaged investigation"
	if err = cfg.Store.Save(bad); err != nil {
		t.Fatal(err)
	}
	rec, err := cfg.Store.Recorder(bad)
	if err != nil {
		t.Fatal(err)
	}
	badPath := rec.Path(session.TurnID(bad.ID, 1))
	if err = os.MkdirAll(filepath.Dir(badPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(badPath, []byte("broken record\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	if _, err = a.eng.Prompt(t.Context(), "current request"); err != nil {
		t.Fatal(err)
	}
	s.Emit(Block{Kind: KindNotice, Text: "Session acceptance: search, cancel, rename, status, safe reopen.", Done: true})
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
				s.post(sessionEvidenceEvent{})
			}
		}
	}()
	capture := func() { s.captureSessionEvidence(other.ID, bad.ID) }
	if mode == "inline" {
		i := &sessionEvidenceInline{newInlineScreen(s, os.Stdout), capture}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&sessionEvidenceScreen{s, capture}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
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

type sessionEvidenceEvent struct{}

func (sessionEvidenceEvent) Timestamp() time.Time { return time.Now() }

type sessionEvidenceScreen struct {
	*screen
	capture func()
}

func (s *sessionEvidenceScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(sessionEvidenceEvent); ok {
		s.capture()
		return nil
	}
	return s.screen.HandleEvent(e)
}

type sessionEvidenceInline struct {
	*inlineScreen
	capture func()
}

func (s *sessionEvidenceInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(sessionEvidenceEvent); ok {
		s.capture()
		return nil
	}
	return s.inlineScreen.HandleEvent(e)
}
func (s *screen) captureSessionEvidence(other, bad string) {
	path := os.Getenv("DASHI_SESSION_STATE")
	if path == "" {
		return
	}
	client := s.eng.cfg.Client.(*fakeClient)
	client.mu.Lock()
	sends := client.sends
	client.mu.Unlock()
	current, _ := s.eng.inspectionMetadata()
	state := struct {
		Input, Session, Title, Other, Damaged, Picker string
		Command, SavedDraft, AnswerDraft, Reason      string
		Approval, Question, Own, Denying, Inspecting  bool
		Prompt                                        uint64
		Mode                                          Mode
		Sends                                         int
		Blocks                                        []Block
		Options, Details, Rows                        []string
	}{Input: s.input, Session: current.ID, Title: current.Title, Other: other, Damaged: bad, Mode: s.ctrl.Mode(), Sends: sends, Blocks: s.main().tr.Blocks, Rows: attachmentRows(&s.attachments, s.ctrl.attachmentRoot)}
	state.SavedDraft, state.Prompt = s.savedDraft, s.promptID
	state.Approval, state.Question, state.Inspecting = s.approval != nil, s.asking != nil, s.dialogStatus != nil
	if s.dialogStatus != nil {
		state.Command = s.dialogStatus.text
	}
	if s.approval != nil {
		state.Denying, state.Reason = s.approval.denying, s.approval.reason
	}
	if s.asking != nil {
		state.Own, state.AnswerDraft = s.asking.own, s.asking.text
	}
	if s.picker != nil {
		state.Picker = s.picker.title
		state.Options = s.picker.options
		state.Details = s.picker.details
	}
	data, _ := json.Marshal(state)
	temp := filepath.Join(filepath.Dir(path), filepath.Base(path)+".tmp")
	if os.WriteFile(temp, data, 0600) == nil {
		os.Rename(temp, path)
	}
}
