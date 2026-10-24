package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
	"golang.org/x/term"
)

// TestTerminalVisualEvidence exports actual Wonton cells for review. It uses
// fixed content and never calls a provider or writes the system clipboard.
func TestTerminalVisualEvidence(t *testing.T) {
	out := os.Getenv("DASHI_TERMINAL_EVIDENCE")
	if out == "" {
		t.Skip("set DASHI_TERMINAL_EVIDENCE to export frames")
	}
	for _, size := range [][2]int{{80, 24}, {40, 16}} {
		for _, state := range []string{"empty", "streaming", "approval", "success", "interrupted", "error", "question"} {
			t.Run(fmt.Sprintf("%s-%dx%d", state, size[0], size[1]), func(t *testing.T) {
				s := terminalVisualFixture(t, state, size[0], size[1])
				saveTerminalFrame(t, out, fmt.Sprintf("%s-%dx%d", state, size[0], size[1]), s.View(), size[0], size[1])
				if state == "streaming" || state == "approval" {
					s.noColor = true
					saveTerminalFrame(t, out, fmt.Sprintf("%s-no-color-%dx%d", state, size[0], size[1]), s.View(), size[0], size[1])
				}
			})
		}
	}
}

func terminalVisualFixture(t *testing.T, state string, width, height int) *screen {
	t.Helper()
	cfg := testConfig(t)
	s := newScreen(cfg.Dir)
	a, err := buildApp(t.Context(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.eng.Close() })
	s.bind(a)
	s.dir = "/Users/developer/projects/dashi"
	s.model = func() string { return "gpt-6-luna" }
	s.width, s.height = width, height
	s.Emit(startupBlock("~/projects/dashi"))
	if state != "empty" {
		s.Emit(Block{Kind: KindUser, Text: "Check the terminal interface.\nMake copying and large pasted prompts easier to use.", Done: true})
		s.Emit(Block{Kind: KindAssistant, Text: "I’ll inspect the composer and transcript, then verify the keyboard controls.", Done: true})
		s.Emit(Block{Kind: KindTool, ID: "tool", Name: "Read", Arg: "cmd/dashi/screen.go", State: StateOK, Elapsed: 120 * time.Millisecond, Lines: []string{"Input, transcript, and approval handling"}, Done: true})
	}
	switch state {
	case "streaming":
		s.ctrl.mode = ModeRunning
		s.Emit(Block{Kind: KindTool, Name: "Bash", Arg: "go test ./cmd/dashi", State: StateRunning, Lines: []string{"Checking terminal interactions…"}})
	case "approval":
		s.promptActive = true
		s.approval = &approvalPrompt{req: toolApproval{ApprovalRequest: ApprovalRequest{Tool: "Bash", Command: "go test ./cmd/dashi", Dir: "~/projects/dashi"}}}
	case "question":
		s.promptActive = true
		s.asking = &askPrompt{q: Question{Prompt: "Which interaction should I check first?", Choices: []workspace.Choice{{Label: "Copying output"}, {Label: "Pasting long prompts"}}}, selected: map[int]bool{}}
	case "success":
		s.Emit(Block{Kind: KindAssistant, Text: "The terminal checks passed.\n\n- Selected text retains its original formatting.\n- Large prompts stay editable before submission.", Done: true})
	case "interrupted":
		s.Emit(Block{Kind: KindStop, Text: "Interrupted. Press Enter to continue.", Done: true})
	case "error":
		s.Emit(Block{Kind: KindTool, Name: "Bash", Arg: "go test ./cmd/dashi", State: StateFailed, Elapsed: time.Second, Lines: []string{"FAIL: expected complete clipboard content"}, Done: true})
	}
	s.drain()
	return s
}

func saveTerminalFrame(t *testing.T, dir, name string, view tui.View, width, height int) {
	t.Helper()
	frame := tui.SprintScreen(view, tui.WithWidth(width), tui.WithHeight(height))
	cells := make([][]any, height)
	for y := range height {
		for x := range width {
			cells[y] = append(cells[y], frame.Cell(x, y))
		}
	}
	data, err := json.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for ext, content := range map[string][]byte{".cells.json": data, ".txt": []byte(frame.Text()), ".ansi": []byte(tui.Sprint(view, tui.WithWidth(width), tui.WithHeight(height), tui.WithColor(true)))} {
		if err = os.WriteFile(filepath.Join(dir, name+ext), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTerminalUserBandPreservesSource(t *testing.T) {
	text := "first line\nsecond line with 世界 and a longer explanation that wraps to the next row"
	view := blockView(Block{Kind: KindUser, Text: text})
	got := tui.SprintScreen(view, tui.WithWidth(40), tui.WithHeight(8)).Text()
	for _, word := range []string{"first line", "second line", "世界"} {
		if !strings.Contains(got, word) {
			t.Fatalf("missing %q: %s", word, got)
		}
	}
}

func TestTerminalBandsStayWithinTheirRows(t *testing.T) {
	s := terminalVisualFixture(t, "streaming", 80, 24)
	frame := tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24))
	lines := strings.Split(frame.Text(), "\n")
	for y, line := range lines {
		if strings.Contains(line, "Check the terminal") || strings.Contains(line, "Make copying") || strings.Contains(line, "Ask Dashi") {
			for x := 2; x < 78; x++ {
				if frame.Cell(x, y).Style.Background.Value != 236 {
					t.Fatalf("prompt background gap at %d,%d", x, y)
				}
			}
		}
		if strings.Contains(line, "I’ll inspect") || strings.Contains(line, "Read") {
			if frame.Cell(75, y).Style.Background.Type != 0 {
				t.Fatalf("prompt background leaked into transcript row %d", y)
			}
		}
	}
}

func TestTerminalNarrowApprovalKeepsEveryChoice(t *testing.T) {
	s := terminalVisualFixture(t, "approval", 40, 16)
	got := tui.SprintScreen(s.View(), tui.WithWidth(40), tui.WithHeight(16)).Text()
	for _, want := range []string{"go test ./cmd/dashi", "in ~/projects/dashi", "[y] Allow once", "[a] Allow for this Dashi run", "[n] Deny", "Esc interrupts", "/help"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in narrow approval:\n%s", want, got)
		}
	}
}

func TestTerminalNoColorPreservesLabels(t *testing.T) {
	for _, inline := range []bool{false, true} {
		s := terminalVisualFixture(t, "approval", 40, 16)
		s.noColor = true
		var view tui.View = s.View()
		if inline {
			view = newInlineScreen(s, io.Discard).LiveView()
		}
		frame := tui.SprintScreen(view, tui.WithWidth(40), tui.WithHeight(16))
		if !strings.Contains(frame.Text(), "[n] Deny") {
			t.Fatal("no-color lost approval")
		}
		for y := range 16 {
			for x := range 40 {
				style := frame.Cell(x, y).Style
				if style.Foreground.Type != 0 || style.Background.Type != 0 {
					t.Fatalf("colored cell in no-color view at %d,%d: %+v", x, y, style)
				}
			}
		}
	}
}

func TestInlineUsesNarrowWidthAndNoEmptyTranscriptRows(t *testing.T) {
	s := terminalVisualFixture(t, "empty", 40, 16)
	i := newInlineScreen(s, io.Discard)
	if i.outputWidth() != 40 {
		t.Fatal("inline widened terminal")
	}
	i.printed = len(s.tr.Blocks)
	_, footerHeight := tui.Measure(i.footer(), 40, 0)
	_, liveHeight := tui.Measure(i.LiveView(), 40, 0)
	if liveHeight != footerHeight {
		t.Fatalf("empty inline transcript retained %d blank rows", liveHeight-footerHeight)
	}
	text, ok := nativeBlockText(Block{Kind: KindUser, Text: strings.Repeat("word ", 20)}, i.outputWidth(), false)
	if !ok {
		t.Fatal("missing native prompt")
	}
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 40 {
			t.Fatalf("native line exceeded width: %q", line)
		}
	}
}

// TestTerminalVisualPTYChild renders fixture states through the real runtimes.
// F10 ends the recording; no prompt is submitted and no provider is called.
func TestTerminalVisualPTYChild(t *testing.T) {
	mode := os.Getenv("DASHI_VISUAL_PTY")
	if mode == "" {
		t.Skip("real terminal visual evidence helper")
	}
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	s := terminalVisualFixture(t, os.Getenv("DASHI_VISUAL_STATE"), width, height)
	s.noColor = os.Getenv("NO_COLOR") != ""
	if mode == "inline" {
		i := &terminalVisualInline{newInlineScreen(s, os.Stdout)}
		err = i.runner.Run(i)
	} else {
		err = tui.Run(&terminalVisualScreen{s}, tui.WithBracketedPaste(true), tui.WithMouseDrag(true), tui.WithFPS(30))
	}
	if err != nil {
		t.Fatal(err)
	}
}

type terminalVisualScreen struct{ *screen }

func (s *terminalVisualScreen) HandleEvent(e tui.Event) []tui.Cmd {
	if key, ok := e.(tui.KeyEvent); ok && key.Key == tui.KeyF10 {
		return []tui.Cmd{tui.Quit()}
	}
	return s.screen.HandleEvent(e)
}

type terminalVisualInline struct{ *inlineScreen }

func (s *terminalVisualInline) HandleEvent(e tui.Event) []tui.Cmd {
	if key, ok := e.(tui.KeyEvent); ok && key.Key == tui.KeyF10 {
		return []tui.Cmd{tui.Quit()}
	}
	return s.inlineScreen.HandleEvent(e)
}

func TestTerminalNarrowToolStatesAndChecks(t *testing.T) {
	for _, test := range []struct {
		state string
		label string
	}{
		{StateChecking, "decide: checking command…"},
		{StateCheckingResult, "decide: checking result…"},
		{StateOK, "1.0s"},
		{StateFailed, "failed  1.0s"},
	} {
		t.Run(test.state, func(t *testing.T) {
			b := Block{Kind: KindTool, Name: "bash", Arg: "curl https://example.com", State: test.state, Elapsed: time.Second,
				Decide: &decideMark{Check: "command", OK: true}, DecideResult: &decideMark{Check: "result", OK: true}}
			frame := tui.SprintScreen(toolView(b), tui.WithWidth(40), tui.WithHeight(8)).Text()
			for _, want := range []string{test.label, "decide ✓ command, result"} {
				if !strings.Contains(frame, want) {
					t.Fatalf("missing %q at 40 columns:\n%s", want, frame)
				}
			}
			if out := os.Getenv("DASHI_TERMINAL_EVIDENCE"); out != "" {
				saveTerminalFrame(t, out, "tool-"+test.state+"-40x8", toolView(b), 40, 8)
			}
		})
	}
	b := Block{Kind: KindTool, Name: "bash", State: StateFailed,
		Decide:       &decideMark{Check: "command", Flagged: true, Text: "severe 90% — flagged"},
		DecideResult: &decideMark{Check: "result", Flagged: true, Text: "injection 99% — flagged, agent warned"}}
	frame := tui.SprintScreen(toolView(b), tui.WithWidth(40), tui.WithHeight(10)).Text()
	words := strings.Join(strings.Fields(frame), " ")
	for _, want := range []string{"failed", "decide command: severe 90% — flagged", "decide result: injection 99% — flagged, agent warned"} {
		if !strings.Contains(words, want) {
			t.Fatalf("missing %q in wrapped checks:\n%s", want, frame)
		}
	}
}

// Enter on an empty composer after /attach sends the selection alone, as
// the attachment list's hint says. Only a cleared line holds it back.
func TestAttachThenEmptyEnterSends(t *testing.T) {
	h := newScreenHarness(t, 0, say("read it"))
	fixtureSelectedText(t, h.dir, "notes.txt", "snapshot")
	h.submit("/attach notes.txt")
	h.waitFor(func() bool {
		return len(h.scr.attachments.entries) == 1 && h.scr.attachments.entries[0].state == attachmentReady
	})
	h.submit("")
	h.app.ctrl.Wait()
	if h.client.sends != 1 {
		t.Fatalf("sends=%d", h.client.sends)
	}
	if docs := hydratedDocuments(h.client.inputAt(0)); len(docs) != 1 || docs[0] != "snapshot" {
		t.Fatalf("documents=%q", docs)
	}
}

// Clearing the composer while a submission is pending leaves the frozen
// entries alone.
func TestKeepEntriesSkipsAFrozenDraft(t *testing.T) {
	d := attachmentDraft{frozenBy: "submit", entries: []*attachmentEntry{{number: 1}}}
	d.keepEntries()
	if d.entries[0].commandOnly || d.cleared {
		t.Fatalf("commandOnly=%v cleared=%v", d.entries[0].commandOnly, d.cleared)
	}
}

// Clearing the composer keeps the staged mentions: a slash command brings
// the draft back, an empty Enter sends nothing, and new text goes with them.
func TestClearedComposerKeepsStagedMentions(t *testing.T) {
	h := newScreenHarness(t, 0, say("summarized"))
	fixtureSelectedText(t, h.dir, "notes.txt", "snapshot")
	const raw = "explain @notes.txt"
	h.submit(raw)
	h.app.ctrl.Wait()
	h.waitFor(func() bool {
		return len(h.scr.attachments.entries) == 1 && h.scr.attachments.entries[0].state == attachmentReady
	})
	clearLine := func() {
		h.mu.Lock()
		h.scr.input = ""
		h.scr.syncDraft("")
		h.mu.Unlock()
	}
	clearLine()
	h.submit("/attachments")
	h.app.ctrl.Wait()
	if !h.check(func() bool { return h.scr.input == raw && len(h.scr.attachments.entries) == 1 }) {
		t.Fatalf("input=%q entries=%d after /attachments", h.scr.input, len(h.scr.attachments.entries))
	}
	clearLine()
	h.submit("")
	h.app.ctrl.Wait()
	if h.client.sends != 0 || !h.check(func() bool { return len(h.scr.attachments.entries) == 1 }) {
		t.Fatalf("empty Enter: sends=%d entries=%d", h.client.sends, len(h.scr.attachments.entries))
	}
	mustHave(t, h.render(), "Type a prompt to send with the selected attachments.")
	const next = "summarize it"
	h.mu.Lock()
	h.scr.input = next
	h.scr.syncDraft(next)
	h.mu.Unlock()
	h.submit(next)
	h.app.ctrl.Wait()
	if h.client.sends != 1 {
		t.Fatalf("sends=%d", h.client.sends)
	}
	if docs := hydratedDocuments(h.client.inputAt(0)); len(docs) != 1 || docs[0] != "snapshot" {
		t.Fatalf("documents=%q", docs)
	}
	// The receipt that clears the draft arrives on another goroutine.
	h.waitFor(func() bool { return len(h.scr.attachments.entries) == 0 })
}
