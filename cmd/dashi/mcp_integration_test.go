package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/wonton/tui"
)

func TestMCPWontonStartupAndInventory(t *testing.T) {
	cfg := testConfig(t)
	docs, docsLog := mcpTestEntry(t)
	broken, brokenLog := mcpTestEntry(t)
	blocked, blockedLog := mcpTestEntry(t)
	broken["env"].(map[string]string)["DASHI_MCP_TEST_FAIL"] = "1"
	docs["env"].(map[string]string)["TOKEN"] = "${DOCS_TOKEN:-SECRET_DEFAULT}"
	docs["args"] = append(docs["args"].([]string), strings.Repeat("long-argument ", 12))
	writeMCPConfig(t, filepath.Join(cfg.Dir, ".mcp.json"), map[string]any{"a_docs": docs, "b_broken": broken, "c_blocked": blocked})
	h := newTranscriptRuntimeConfig(t, cfg)
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeApproval && strings.Contains(s.text, "Start MCP server a_docs?") && strings.Contains(s.text, "Do not start")
	})
	// All details remain scrollable while decisions stay on-screen.
	h.key(tui.KeyEvent{Key: tui.KeyEnd})
	h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "${DOCS_TOKEN:-<redacted>}") && strings.Contains(s.text, "Start for this Dashi run")
	})
	h.key(tui.KeyEvent{Key: tui.KeyArrowUp})
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Start MCP server b_broken?") })
	h.key(tui.KeyEvent{Key: tui.KeyArrowUp})
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "Start MCP server c_blocked?") })
	h.key(tui.KeyEvent{Key: tui.KeyEnter}) // default is decline
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "1 connected, 1 failed, 1 blocked")
	})
	h.typeText("/mcp")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "c_blocked: blocked")
	})
	h.key(tui.KeyEvent{Key: tui.KeyHome, Ctrl: true})
	h.wait(func(s transcriptState) bool {
		return strings.Contains(s.text, "a_docs: connected") || strings.Contains(s.text, "MCP servers")
	})
	if h.app.eng.Saved() {
		t.Fatal("inventory recorded a Turn")
	}
	if _, err := os.Stat(blockedLog); !os.IsNotExist(err) {
		t.Fatal("declined server started")
	}
	if strings.Contains(h.app.eng.cfg.MCP.Inventory(), "SECRET_DEFAULT") {
		t.Fatal("literal secret leaked")
	}
	h.app.wiring.Close()
	mcpAssertReaped(t, docsLog)
	mcpAssertReaped(t, brokenLog)
}
func TestMCPWontonStartupCancelNoTurnNoChild(t *testing.T) {
	cfg := testConfig(t)
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(cfg.Dir, ".mcp.json"), map[string]any{"docs": entry})
	h := newTranscriptRuntimeConfig(t, cfg)
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeApproval && strings.Contains(s.text, "Start MCP server docs?")
	})
	h.key(tui.KeyEvent{Key: tui.KeyCtrlC})
	h.app.ctrl.Wait()
	if h.app.eng.Saved() {
		t.Fatal("canceled startup recorded Turn")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("canceled child started")
	}
}
func TestMCPCommandDiscoveryAndNoInventoryEffects(t *testing.T) {
	h := newHarness(t)
	help := h.app.ctrl.commandHelp()
	if !strings.Contains(help, "/mcp") || !strings.Contains(help, "  "+duringMarker+" /mcp ") {
		t.Fatal(help)
	}
	if !strings.Contains(strings.Join(h.app.ctrl.commandNames(), ","), "/mcp") {
		t.Fatal("completion missing MCP")
	}
	h.app.ctrl.Input("/mcp")
	h.app.ctrl.Wait()
	mustHave(t, h.ui.text(), "None configured")
	if h.turns() != 0 {
		t.Fatal("inventory dispatched a Turn")
	}
	h.app.ctrl.Input("/mcp invalid")
	mustHave(t, h.ui.text(), "Usage: /mcp")
}
func TestMCPPrintAndPipedModesStartupBeforePrompt(t *testing.T) {
	for _, mode := range []string{"print", "pipe"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t, say("Ready."))
			entry, log := mcpTestEntry(t)
			writeMCPConfig(t, filepath.Join(cfg.Dir, ".mcp.json"), map[string]any{"docs": entry})
			var out, stderr bytes.Buffer
			old := isTerminal
			isTerminal = func(*os.File) bool { return false }
			defer func() { isTerminal = old }()
			code := 0
			if mode == "print" {
				code = runPrint(t.Context(), cfg, Options{Prompt: "work", Run: true, Yes: true}, &out, &stderr)
			} else {
				code = runLines(t.Context(), cfg, Options{Plain: true}, strings.NewReader("work\n/quit\n"), &out, &stderr)
			}
			if code != 0 || !strings.Contains(out.String(), "Ready.") {
				t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("unattended project server started")
			}
			if cfg.Client.(*fakeClient).sends != 1 {
				t.Fatal("piped prompt was dropped or duplicated")
			}
		})
	}
}
func TestMCPReopenApprovalDoesNotRepeatFinishedSearch(t *testing.T) {
	cfg := testConfig(t, callTool("search", "mcp__docs__search", `{}`), callTool("fetch", "mcp__docs__fetch", `{}`), say("Fetched after reopen."))
	entry, log := mcpTestEntry(t)
	writeMCPConfig(t, filepath.Join(cfg.Dir, ".mcp.json"), map[string]any{"docs": entry})
	os.WriteFile(filepath.Join(cfg.Dir, "guide.md"), []byte("immutable recovery guide"), 0600)
	ui := newRecorderUI()
	asked := make(chan struct{})
	ui.answer = func(ctx context.Context, r ApprovalRequest) (Approval, error) {
		if r.Tool == "mcp__docs__fetch" {
			close(asked)
			<-ctx.Done()
			return Approval{}, ctx.Err()
		}
		return Approval{Allow: true}, nil
	}
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.openSession(cfg.Store, cfg.Dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	consent := func(context.Context, mcpStartupRequest) (bool, error) { return true, nil }
	if err = a.eng.cfg.MCP.Initialize(t.Context(), consent); err != nil {
		t.Fatal(err)
	}
	a.ctrl.Input("find the recovery guide")
	<-asked
	a.ctrl.QuitDuringRun("/quit")
	a.ctrl.Wait()
	id := a.eng.Session().ID
	a.eng.Close()
	a.wiring.Close()
	mcpAssertReaped(t, log)
	first, _ := os.ReadFile(log)
	ui = newRecorderUI()
	a, err = buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	defer a.wiring.Close()
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: id}); err != nil {
		t.Fatal(err)
	}
	if err = a.eng.cfg.MCP.Initialize(t.Context(), consent); err != nil {
		t.Fatal(err)
	}
	res, err := a.eng.Resume(t.Context())
	if err != nil || res.Outcome.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), "call search") != 1 || strings.Count(string(data), "call fetch") != 1 || strings.Count(string(first), "call fetch") != 0 {
		t.Fatal(string(data))
	}
	if len(ui.approvals) != 1 || ui.approvals[0].Tool != "mcp__docs__fetch" {
		t.Fatalf("approvals=%+v", ui.approvals)
	}
	tools, _ := a.eng.cfg.MCP.Tools(t.Context())
	for _, tl := range tools {
		if tl.Definition.Annotations.ReadOnly != nil || tl.Definition.Annotations.Idempotent != nil {
			t.Fatal("server hints widened repeat permission")
		}
	}
	if errors.Is(err, tool.ErrFatal) {
		t.Fatal(err)
	}
}

func TestMCPWontonInventoryEmpty(t *testing.T) {
	h := newTranscriptRuntime(t)
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeIdle && strings.Contains(s.text, "Type a message...")
	})
	h.typeText("/mc")
	h.key(tui.KeyEvent{Key: tui.KeyTab})
	h.wait(func(s transcriptState) bool { return s.input == "/mcp" })
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool { return strings.Contains(s.text, "None configured.") })
	if h.app.eng.Saved() {
		t.Fatal("empty inventory recorded work")
	}
}
func TestMCPWontonStartupPreservesRejectedDraft(t *testing.T) {
	cfg := testConfig(t)
	entry, _ := mcpTestEntry(t)
	entry["env"].(map[string]string)["DASHI_MCP_TEST_START_DELAY"] = "600"
	writeMCPConfig(t, filepath.Join(cfg.HomeDir, ".dashi", "settings.json"), map[string]any{"docs": entry})
	h := newTranscriptRuntimeConfig(t, cfg)
	h.wait(func(s transcriptState) bool {
		return s.mode == ModeCommand && strings.Contains(s.text, "Starting MCP servers")
	})
	h.typeText("kept draft")
	h.key(tui.KeyEvent{Key: tui.KeyEnter})
	h.wait(func(s transcriptState) bool {
		return s.input == "kept draft" && strings.Contains(s.text, "Submit this input again")
	})
	h.wait(func(s transcriptState) bool { return s.mode == ModeIdle && s.input == "kept draft" })
	if cfg.Client.(*fakeClient).sends != 0 || h.app.eng.Queued() != 0 || h.app.eng.Saved() {
		t.Fatal("startup accepted input")
	}
}
