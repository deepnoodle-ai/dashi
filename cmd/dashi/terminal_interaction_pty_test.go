package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// TestTerminalInteractionPTYChild is driven by the external evidence harness.
// It uses actual terminal input, a deterministic model, and an injected
// clipboard. No provider or system clipboard is accessed. Normal tests skip it.
func TestTerminalInteractionPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_INTERACTION_PTY")
	if mode == "" {
		t.Skip("real terminal interaction evidence helper")
	}
	if mode != "fullscreen" && mode != "inline" {
		t.Fatal("DASHI_INTERACTION_PTY must be fullscreen or inline")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires PTY stdin")
	}
	before, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, say("Received exactly one submitted prompt."))
	cfg.Client.(*fakeClient).window = 2_000_000
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
	capture := &terminalInteractionCapture{}
	s.clipboardWrite = func(_ context.Context, text string) (clipboardWriteResult, error) {
		capture.mu.Lock()
		capture.clipboard, capture.copies = text, capture.copies+1
		capture.mu.Unlock()
		return clipboardWriteResult{notice: "Fixture copied exact source; system clipboard untouched.", confirmed: true}, nil
	}
	s.Emit(Block{Kind: KindAssistant, Text: "Clipboard **proof** 世界\t" + strings.Repeat("wrapped ", 16) + "END\n\n```text\ncopy code 世界\tkeep spaces  \nsecond code line\n```", Done: true})
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.post(terminalInteractionEvent{})
			}
		}
	}()
	if mode == "inline" {
		i := &terminalInteractionInline{newInlineScreen(s, os.Stdout), capture}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&terminalInteractionScreen{s, capture}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	after, err := composerTTYAttributes(int(os.Stdin.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("terminal attributes were not restored", err)
	}
	capture.write(s)
}

type terminalInteractionCapture struct {
	mu        sync.Mutex
	clipboard string
	copies    int
}

type terminalInteractionEvent struct{}

func (terminalInteractionEvent) Timestamp() time.Time { return time.Now() }

type terminalInteractionScreen struct {
	*screen
	capture *terminalInteractionCapture
}

func (s *terminalInteractionScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(terminalInteractionEvent); ok {
		s.capture.write(s.screen)
		return nil
	}
	return s.screen.HandleEvent(e)
}

type terminalInteractionInline struct {
	*inlineScreen
	capture *terminalInteractionCapture
}

func (s *terminalInteractionInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(terminalInteractionEvent); ok {
		s.capture.write(s.screen)
		return nil
	}
	return s.inlineScreen.HandleEvent(e)
}

type terminalTextProof struct {
	Bytes int    `json:"bytes"`
	Lines int    `json:"lines"`
	Hash  string `json:"hash"`
}

func terminalProof(text string) terminalTextProof {
	hash := sha256.Sum256([]byte(text))
	return terminalTextProof{len(text), strings.Count(text, "\n") + 1, hex.EncodeToString(hash[:])}
}

func (c *terminalInteractionCapture) write(s *screen) {
	path := os.Getenv("DASHI_INTERACTION_STATE")
	if path == "" {
		return
	}
	state := struct {
		Draft, Selected, Clipboard, Submitted             terminalTextProof
		Copies, Sends, Prepared, Width, Height, ViewportY int
		Mode                                              Mode
		Mouse, FileWarning                                bool
	}{Draft: terminalProof(s.input), Selected: terminalProof(s.main().vp.SelectedText()), Mode: s.ctrl.Mode(), Mouse: s.mouseEnabled, Width: s.width, Height: s.height, ViewportY: s.main().vp.Y}
	c.mu.Lock()
	state.Clipboard, state.Copies = terminalProof(c.clipboard), c.copies
	c.mu.Unlock()
	client := s.eng.cfg.Client.(*fakeClient)
	client.mu.Lock()
	state.Sends, state.Prepared = client.sends, len(client.inputs)
	if len(client.inputs) > 0 {
		messages := client.inputs[len(client.inputs)-1].Messages
		for i := len(messages) - 1; i >= 0; i-- {
			// Runtime reminders may follow the prompt in user-role messages.
			if messages[i].Role == llm.RoleUser && messages[i].Text() != "" {
				state.Submitted = terminalProof(messages[i].Text())
				break
			}
		}
	}
	client.mu.Unlock()
	for _, block := range s.main().tr.Blocks {
		if strings.Contains(block.Text, "file-selection limit") {
			state.FileWarning = true
		}
	}
	data, _ := json.Marshal(state)
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}
