package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// This helper is driven through a real 80x24 PTY by the evidence harness.
// It never makes provider calls. Set DASHI_COMPOSER_PTY to fullscreen or inline.
func TestComposerPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_COMPOSER_PTY")
	if mode == "" {
		t.Skip("real editor evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires real PTY stdin")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
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
	h, entries := openInputHistory(filepath.Join(cfg.HomeDir, ".dashi", "history.json"), func(err error) { s.Emit(warnBlock("%v", err)) })
	s.historyStore, s.history = h, entries
	defer h.close()
	s.pathCompletion = indexWorkspace(cfg.Dir)
	s.Emit(Block{Kind: KindNotice, Text: "Composer real editor acceptance", Done: true})
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
				s.post(composerEvidenceEvent{})
			}
		}
	}()
	if mode == "inline" {
		i := &composerEvidenceInline{inlineScreen: newInlineScreen(s, os.Stdout)}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&composerEvidenceScreen{screen: s}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	h.close()
	if err != nil {
		t.Fatal(err)
	}
	after, stateErr := composerTTYAttributes(int(os.Stdin.Fd()))
	if stateErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("terminal attributes were not restored: %v before=%#v after=%#v", stateErr, before, after)
	}
	if cfg.Client.(*fakeClient).sends != 0 {
		t.Fatal("editor return submitted a model request")
	}
}

type composerEvidenceEvent struct{}

func (composerEvidenceEvent) Timestamp() time.Time { return time.Now() }

type composerEvidenceScreen struct{ *screen }

func (s *composerEvidenceScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(composerEvidenceEvent); ok {
		s.captureComposerEvidence()
		return nil
	}
	return s.screen.HandleEvent(e)
}

type composerEvidenceInline struct{ *inlineScreen }

func (i *composerEvidenceInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(composerEvidenceEvent); ok {
		i.captureComposerEvidence()
		return nil
	}
	return i.inlineScreen.HandleEvent(e)
}
func (s *screen) captureComposerEvidence() {
	path := os.Getenv("DASHI_COMPOSER_STATE")
	if path == "" {
		return
	}
	state := struct {
		Input  string
		Blocks []Block
		Mode   Mode
	}{s.input, s.tr.Blocks, s.ctrl.Mode()}
	data, _ := json.Marshal(state)
	temp := path + ".tmp"
	if os.WriteFile(temp, data, 0600) == nil {
		os.Rename(temp, path)
	}
}

func TestComposerInlineFocused(t *testing.T) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("requires a PTY for InlineApp startup")
	}
	cfg := testConfig(t, say("Accepted."))
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
	i := &runtimeInline{inlineScreen: newInlineScreen(s, os.Stdout), ready: make(chan struct{})}
	i.runner = tui.NewInlineApp(tui.WithInlineInput(strings.NewReader("")), tui.WithInlineOutput(io.Discard), tui.WithInlineWidth(80), tui.WithInlineFPS(60))
	s.send = i.runner.SendEvent
	done := make(chan error, 1)
	go func() { done <- i.runner.Run(i) }()
	defer func() {
		a.ctrl.CancelWork()
		a.ctrl.Wait()
		i.runner.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-i.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("inline startup")
	}
	key := func(event tui.KeyEvent) { i.runner.SendEvent(event) }
	wait := func(want string) {
		deadline := time.After(3 * time.Second)
		for {
			probe := inlineProbe{reply: make(chan inlineState, 1)}
			i.runner.SendEvent(probe)
			select {
			case state := <-probe.reply:
				if state.input == want {
					return
				}
			case <-deadline:
				t.Fatalf("inline draft did not become %q", want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	typeText := func(text string) {
		for _, r := range text {
			key(tui.KeyEvent{Rune: r})
		}
	}
	typeText("/m")
	key(tui.KeyEvent{Key: tui.KeyTab})
	key(tui.KeyEvent{Key: tui.KeyEscape})
	wait("/m")
	key(tui.KeyEvent{Key: tui.KeyHome})
	key(tui.KeyEvent{Key: tui.KeyCtrlD})
	wait("m")
	key(tui.KeyEvent{Key: tui.KeyEnd})
	key(tui.KeyEvent{Key: tui.KeyCtrlU})
	wait("")
	raw := "世界\nsecond\tline"
	key(tui.KeyEvent{Paste: raw})
	wait(raw)
	key(tui.KeyEvent{Key: tui.KeyEnter})
	a.ctrl.Wait()
	wait("")
	typeText("draft")
	key(tui.KeyEvent{Key: tui.KeyArrowUp})
	wait(raw)
	key(tui.KeyEvent{Key: tui.KeyEnd, Ctrl: true})
	key(tui.KeyEvent{Key: tui.KeyArrowDown})
	wait("draft")
	key(tui.KeyEvent{Key: tui.KeyCtrlC})
	wait("draft")
	typeText(" kept")
	key(tui.KeyEvent{Key: tui.KeyCtrlC})
	wait("draft kept")
}
