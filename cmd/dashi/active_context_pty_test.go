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

// Driven by the evidence harness through real 80x24 PTY input in both Wonton
// modes. No credentials or provider calls are used.
func TestActiveContextPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_ACTIVE_CONTEXT_PTY")
	if mode == "" {
		t.Skip("real PTY evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires real PTY stdin")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg, c := activeFixtureConfig(t, true)
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
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				s.post(activeEvidenceEvent{})
			}
		}
	}()
	if mode == "inline" {
		i := &activeEvidenceInline{inlineScreen: newInlineScreen(s, os.Stdout)}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&activeEvidenceScreen{screen: s}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	after, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("TTY attributes not restored: %v", err)
	}
	activeAssertCompleted(t, cfg, a, c)
	if dir := os.Getenv("DASHI_ACTIVE_CONTEXT_EVIDENCE"); dir != "" {
		c.mu.Lock()
		counts := struct {
			Ordinary, Summaries int
			TTYRestored         bool
		}{c.ordinary, c.summaries, true}
		c.mu.Unlock()
		data, _ := json.MarshalIndent(counts, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, mode+"-proof.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type activeEvidenceEvent struct{}

func (activeEvidenceEvent) Timestamp() time.Time { return time.Now() }

type activeEvidenceScreen struct{ *screen }

func (s *activeEvidenceScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(activeEvidenceEvent); ok {
		s.captureActiveEvidence()
		return nil
	}
	return s.screen.HandleEvent(e)
}

type activeEvidenceInline struct{ *inlineScreen }

func (i *activeEvidenceInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(activeEvidenceEvent); ok {
		i.captureActiveEvidence()
		return nil
	}
	return i.inlineScreen.HandleEvent(e)
}
func (s *screen) captureActiveEvidence() {
	path := os.Getenv("DASHI_ACTIVE_CONTEXT_STATE")
	if path == "" {
		return
	}
	state := struct {
		Session      string
		Mode         Mode
		Approval     bool
		ApprovalTool string
		Picker       string
		Blocks       []Block
	}{Session: s.eng.Session().ID, Mode: s.ctrl.Mode(), Approval: s.approval != nil, Blocks: s.main().tr.Blocks}
	if s.approval != nil {
		state.ApprovalTool = s.approval.req.Tool
	}
	if s.picker != nil {
		state.Picker = s.picker.title
	}
	data, _ := json.Marshal(state)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}
