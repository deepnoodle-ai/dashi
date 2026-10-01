package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// Keyless production dialogs driven by terminal bytes, with recorded outcomes
// checked only after the operator deliberately answers the original prompt.
func TestDialogStatusPTYChild(t *testing.T) {
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
	cfg := inspectionTestConfig(t,
		say("Initial recorded answer."),
		callTool("approve", workspace.ToolWriteFile, `{"path":"one.txt","content":"approved once"}`), say("Saved after deliberate approval."),
		callTool("free", workspace.ToolAskUser, `{"question":"Describe the fix"}`), say("Received deliberate answer."),
		callTool("choices", workspace.ToolAskUser, `{"question":"Choose the target","multiple":true,"choices":[{"label":"one"},{"label":"two"}]}`), say("Received selected target."))
	cfg.Dir = os.Getenv("DASHI_SESSION_WORKSPACE")
	other := recordedInspectionSession(t, cfg, "other inspection target", true)
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
	if _, err = a.eng.Prompt(t.Context(), "initial request"); err != nil {
		t.Fatal(err)
	}
	s.input = "unsent main draft"
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
	started := false
	capture := func() {
		if !started {
			started = true
			a.ctrl.Input("approve fixture")
		}
		s.captureSessionEvidence(other.ID, "")
	}
	if mode == "inline" {
		i := &sessionEvidenceInline{newInlineScreen(s, os.Stdout), capture}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&sessionEvidenceScreen{s, capture}, tui.WithBracketedPaste(true), tui.WithFPS(30))
	}
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	after, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal attributes not restored", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Dir, "one.txt"))
	if err != nil || string(data) != "approved once" {
		t.Fatal("deliberate approval failed", err)
	}
	for n, want := range map[int]string{3: "deliberate answer", 4: `{"selections":["two"]}`} {
		rec, err := cfg.Store.Recorder(a.eng.Session())
		if err != nil {
			t.Fatal(err)
		}
		turn, err := rec.Load(t.Context(), dashi.TurnID(a.eng.Session().ID, n))
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, step := range turn.Steps {
			if step.Tool != nil && step.Tool.Call.Name == workspace.ToolAskUser && step.Tool.Outcome != nil {
				got = outputText(step.Tool.Outcome)
			}
		}
		if got != want {
			t.Fatalf("Turn %d recorded answer %q, want %q", n, got, want)
		}
	}
}
