package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// TestMCPPTYChild runs the real Wonton UI in the evidence harness's 80x24 PTY.
// Model replies are scripted; stdio startup/search/fetch and coding are real.
func TestMCPPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_MCP_PTY")
	if mode == "" {
		t.Skip("real terminal evidence helper")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("requires real PTY stdin")
	}
	cfg := testConfig(t, callTool("write", "write_file", `{"path":"note.txt","content":"Recovery note"}`), callTool("search", "mcp__a_docs__search", `{"query":"recovery"}`), callTool("fetch", "mcp__a_docs__fetch", `{"path":"guide.md"}`), say("Used the fetched recovery guide and wrote note.txt."))
	docs, docsLog := mcpTestEntry(t)
	broken, brokenLog := mcpTestEntry(t)
	blocked, blockedLog := mcpTestEntry(t)
	docs["env"].(map[string]string)["TOKEN"] = "${DOCS_TOKEN:-SECRET_DEFAULT}"
	docs["env"].(map[string]string)["DASHI_MCP_TEST_START_DELAY"] = "1200"
	broken["env"].(map[string]string)["DASHI_MCP_TEST_FAIL"] = "1"
	writeMCPConfig(t, filepath.Join(cfg.Dir, ".mcp.json"), map[string]any{"a_docs": docs, "b_broken": broken, "c_blocked": blocked})
	if err := os.WriteFile(filepath.Join(cfg.Dir, "guide.md"), []byte("Recorded facts preserve the recovery boundary."), 0600); err != nil {
		t.Fatal(err)
	}
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	defer a.wiring.Close()
	s.bind(a)
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				s.post(mcpEvidenceEvent{})
			}
		}
	}()
	if mode == "inline" {
		i := &mcpEvidenceInline{inlineScreen: newInlineScreen(s, os.Stdout)}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&mcpEvidenceScreen{screen: s}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	a.ctrl.CancelWork()
	a.ctrl.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.wiring.Close(); err != nil {
		t.Fatal(err)
	}
	mcpAssertReaped(t, docsLog)
	mcpAssertReaped(t, brokenLog)
	if _, err = os.Stat(blockedLog); !os.IsNotExist(err) {
		t.Fatal("declined server started")
	}
	data, err := os.ReadFile(filepath.Join(cfg.Dir, "note.txt"))
	if err != nil || string(data) != "Recovery note" {
		t.Fatalf("coding result=%s err=%v", data, err)
	}
}

type mcpEvidenceEvent struct{}

func (mcpEvidenceEvent) Timestamp() time.Time { return time.Now() }

type mcpEvidenceScreen struct{ *screen }

func (s *mcpEvidenceScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(mcpEvidenceEvent); ok {
		s.captureMCPState()
		return nil
	}
	return s.screen.HandleEvent(e)
}

type mcpEvidenceInline struct{ *inlineScreen }

func (i *mcpEvidenceInline) HandleEvent(e tui.Event) []tui.Cmd {
	if _, ok := e.(mcpEvidenceEvent); ok {
		i.captureMCPState()
		return nil
	}
	return i.inlineScreen.HandleEvent(e)
}
func (s *screen) captureMCPState() {
	path := os.Getenv("DASHI_MCP_STATE")
	if path == "" {
		return
	}
	state := struct {
		Input, PickerTitle, ApprovalTool, Inventory string
		Mode                                        Mode
		Blocks                                      []Block
	}{Input: s.input, Mode: s.ctrl.Mode(), Blocks: s.tr.Blocks, Inventory: s.eng.cfg.MCP.Inventory()}
	if s.picker != nil {
		state.PickerTitle = s.picker.title
	}
	if s.approval != nil {
		state.ApprovalTool = s.approval.req.Tool
	}
	data, _ := json.Marshal(state)
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		os.Rename(path+".tmp", path)
	}
}
