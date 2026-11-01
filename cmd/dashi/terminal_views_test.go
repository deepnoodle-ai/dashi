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
	"github.com/deepnoodle-ai/wonton/runewidth"
	"github.com/deepnoodle-ai/wonton/termtest"
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
		s.Emit(Block{Kind: KindTool, ID: "tool", Name: "read_file", Arg: "cmd/dashi/screen.go", Label: newToolLabel("read_file", json.RawMessage(`{"path":"cmd/dashi/screen.go"}`), ""), State: StateOK, Elapsed: 120 * time.Millisecond, Lines: []string{"Input, transcript, and approval handling"}, Done: true})
	}
	switch state {
	case "streaming":
		s.ctrl.mode = ModeRunning
		s.Emit(Block{Kind: KindTool, Name: "bash", Arg: "go test ./cmd/dashi", Input: "$ go test ./cmd/dashi", State: StateRunning, Lines: []string{"Checking terminal interactions…"}})
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
		s.Emit(Block{Kind: KindTool, Name: "bash", Arg: "go test ./cmd/dashi", Input: "$ go test ./cmd/dashi", State: StateFailed, Elapsed: time.Second, Lines: []string{"FAIL: expected complete clipboard content"}, Done: true})
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
	for _, want := range []string{"go test ./cmd/dashi", "in ~/projects/dashi", "y Allow once", "a Allow for this Dashi run", "n Deny", "Esc interrupts", "/help"} {
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
		if !strings.Contains(frame.Text(), "n Deny") {
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
	i.printed = len(s.main().tr.Blocks)
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

// dialogFixtures builds each dialog that replaces the composer, at width.
func dialogFixtures(t *testing.T, width int) map[string]func() tui.View {
	t.Helper()
	s := terminalVisualFixture(t, "empty", width, 40)
	done := make(chan struct{})
	close(done)
	flagged := &pendingCheck{done: done, result: decideResult{Checked: true, Flagged: true, Severe: 0.91}}
	bash := toolApproval{ApprovalRequest: ApprovalRequest{Tool: "bash", Command: "rm -rf build"}, Check: flagged}
	write := toolApproval{ApprovalRequest: ApprovalRequest{Tool: "write_file", Summary: "write notes.md", RememberScope: "This tool",
		Arguments: `{"path":"notes.md","content":"` + strings.Repeat("A long line of notes that wraps across the band. ", 6) + `"}`}}
	question := Question{Prompt: "Which interaction should I check first?", Choices: []workspace.Choice{
		{Label: "Copying output", Description: "Selected text keeps its formatting."}, {Label: "Pasting long prompts"}}}
	return map[string]func() tui.View{
		"approval-flagged": func() tui.View {
			s.approval = &approvalPrompt{req: bash}
			return s.approvalView()
		},
		"approval-remember": func() tui.View {
			s.approval = &approvalPrompt{req: write}
			return s.approvalView()
		},
		"approval-deny": func() tui.View {
			s.approval = &approvalPrompt{req: bash, denying: true, reason: "use make clean"}
			return s.approvalView()
		},
		"question": func() tui.View {
			s.asking = &askPrompt{q: question, selected: map[int]bool{}}
			return s.questionView()
		},
		"question-own": func() tui.View {
			s.asking = &askPrompt{q: question, selected: map[int]bool{}, own: true, err: "Enter an answer before submitting."}
			return s.questionView()
		},
		"picker": func() tui.View {
			return pickerView(&pickerState{title: "Choose a model", options: []string{"gpt-6-luna", "claude-opus-5-5", "gemini-3-pro"}})
		},
		"picker-notes": func() tui.View {
			return pickerView(&pickerState{title: "Choose a model", options: []string{"openai  gpt-6-luna", "anthropic  claude-opus-5-5", "All models…"},
				notes: []string{"1.1M · current", "1M", "90 more"}, footer: "Set GEMINI_API_KEY to add google models"})
		},
		"session-picker": func() tui.View {
			return pickerView(&pickerState{title: "Workspace sessions", sessionCards: true, options: []string{"Fix the composer · 2h ago", "Review the theme · yesterday"},
				details: []string{"Session s_abc123\nFirst request: fix the composer so pasted prompts stay editable", "Session s_def456"}})
		},
		"mcp-startup": func() tui.View {
			return pickerView(&pickerState{title: "Start MCP server docs?", options: []string{"Start for this Dashi run", "Do not start"}, index: 1,
				startupReview: detailLines{"Command: npx docs-mcp", "Source: .dashi/mcp.json", "Environment: DOCS_TOKEN"}})
		},
		"dialog-status": func() tui.View {
			s.asking = nil
			s.approval = &approvalPrompt{req: bash}
			s.dialogStatus = &dialogStatusInput{text: "/tasks"}
			defer func() { s.dialogStatus = nil }()
			return s.dialogStatusView()
		},
	}
}

// TestDialogsDrawOnTheBand checks that every cell of a dialog's band carries
// the band background, including wrapped text, viewports, and input fields,
// and that the hints below the band do not.
func TestDialogsDrawOnTheBand(t *testing.T) {
	const width = 100
	for name, build := range dialogFixtures(t, width) {
		frame := tui.SprintScreen(build(), tui.WithWidth(width))
		_, height := frame.Size()
		onBand := func(x, y int) bool {
			bg := frame.Cell(x, y).Style.Background
			return bg.Type == termtest.Color256 && bg.Value == 236
		}
		rows := 0
		for rows < height && onBand(0, rows) {
			for x := range width {
				if !onBand(x, rows) {
					t.Fatalf("%s: cell %d,%d is off the band:\n%s", name, x, rows, frame.Text())
				}
			}
			rows++
		}
		if rows < 3 || rows == height || onBand(0, rows) {
			t.Fatalf("%s: band covers %d of %d rows:\n%s", name, rows, height, frame.Text())
		}
		if title := frame.Row(1); !strings.HasPrefix(title, " ? ") && !strings.HasPrefix(title, " ◆ ") {
			t.Fatalf("%s: title row %q has no glyph", name, title)
		}
	}
}

// toolRowFixture is a screen whose transcript holds a finished bash row, a
// failed one, and a running one, each with input and output.
func toolRowFixture() *screen {
	s := newScreen("fixture")
	addToolRows(s)
	return s
}

func addToolRows(s *screen) {
	s.width, s.height = 80, 24
	bash := newToolLabel("bash", json.RawMessage(`{"command":"go test ./..."}`), "")
	s.main().tr.Apply(Block{ID: "ok", Kind: KindTool, Name: "bash", Arg: "go test ./...", Label: bash, Input: "$ go test ./...",
		State: StateOK, Elapsed: 1200 * time.Millisecond, Summary: "PASS", Detail: "ok  pkg\nPASS\n", Done: true})
	s.main().tr.Apply(Block{ID: "failed", Kind: KindTool, Name: "bash", Arg: "make lint", Label: bash, Input: "$ make lint",
		State: StateFailed, Elapsed: 3200 * time.Millisecond, Summary: "exit 2 · undefined: foo", Detail: "undefined: foo\n[Exit code 2]", Done: true})
	s.main().tr.Apply(Block{ID: "running", Kind: KindTool, Name: "bash", Arg: "sleep 9", Label: bash, Input: "$ sleep 9",
		State: StateRunning, Lines: []string{"tick 1", "tick 2"}})
}

func TestToolRowHidesInputAndOutputUntilExpanded(t *testing.T) {
	s := toolRowFixture()
	row := func(i int) string { return tui.Sprint(s.main().Item(i), tui.WithWidth(78), tui.WithColor(false)) }
	for i, want := range []string{"● Ran a command", "✗ Ran a command", spinnerFrames[0] + " Running a command…"} {
		got := row(i)
		if !strings.HasPrefix(got, want) || strings.Contains(got, "\n") {
			t.Fatalf("collapsed row %d=%q, want one line starting %q", i, got, want)
		}
		for _, hidden := range []string{"go test", "make lint", "sleep", "PASS", "undefined", "tick", "└", "click"} {
			if strings.Contains(got, hidden) {
				t.Fatalf("collapsed row %d=%q shows %q", i, got, hidden)
			}
		}
	}
	if got := row(1); !strings.HasSuffix(got, "failed  3.2s") {
		t.Fatalf("failed row=%q", got)
	}
	if styled := tui.Sprint(s.main().Item(1), tui.WithWidth(78), tui.WithColor(true)); !strings.Contains(styled, "\x1b[0;31mfailed") {
		t.Fatalf("failed state style=%q", styled)
	}
	if src, _ := s.main().Source(0); src != "Ran a command" {
		t.Fatalf("collapsed source=%q", src)
	}

	s.main().expanded = map[int]bool{0: true, 1: true, 2: true}
	for i, want := range []string{
		"Input\n  $ go test ./...\n  Output\n  ok  pkg\n  PASS",
		"Input\n  $ make lint\n  Output\n  undefined: foo\n  [Exit code 2]",
		"Input\n  $ sleep 9\n  Output\n  tick 1\n  tick 2",
	} {
		if got := row(i); !strings.Contains(got, want) {
			t.Fatalf("expanded row %d=%q, want %q", i, got, want)
		}
	}
	// The headings are decoration; a selection copies the label, the
	// input, and the output.
	if src, _ := s.main().Source(0); src != "Ran a command\n$ go test ./...\nok  pkg\nPASS" {
		t.Fatalf("expanded source=%q", src)
	}
}

func TestClickTogglesAToolRow(t *testing.T) {
	h := newScreenHarness(t, 0)
	s := h.scr
	h.mu.Lock()
	defer h.mu.Unlock()
	addToolRows(s)
	frame := func() string {
		return tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24)).Text()
	}
	click := func(text string) {
		x, y := cellOf(t, strings.Split(frame(), "\n"), text)
		s.HandleEvent(tui.MouseEvent{Type: tui.MouseClick, Button: tui.MouseButtonLeft, ClickCount: 1, X: x, Y: y})
	}
	click("Ran a command")
	if got := frame(); !strings.Contains(got, "$ go test ./...") || strings.Contains(got, "$ make lint") {
		t.Fatalf("click did not expand only the first row:\n%s", got)
	}
	// A click in the output keeps the row open; a click on its head closes it.
	click("ok  pkg")
	if !strings.Contains(frame(), "ok  pkg") {
		t.Fatal("a click in the output collapsed the row")
	}
	click("Ran a command")
	if got := frame(); strings.Contains(got, "go test") {
		t.Fatalf("second click did not collapse the row:\n%s", got)
	}
	// A row with no input and no output has nothing to expand.
	if toolExpandable(Block{Kind: KindTool, Name: "task", State: StateOK}) {
		t.Fatal("empty row is expandable")
	}
}

// Transcript text and tool rows take the whole terminal width.
func TestRowsSpanTheWholeWidth(t *testing.T) {
	s := toolRowFixture()
	s.width = 160
	s.main().tr.Apply(Block{ID: "a", Kind: KindAssistant, Text: strings.Repeat("word ", 60), Done: true})
	head := tui.Sprint(s.main().Item(1), tui.WithWidth(158), tui.WithColor(false))
	if runewidth.StringWidth(head) != 158 || !strings.HasSuffix(head, "failed  3.2s") {
		t.Fatalf("tool row is %d columns: %q", runewidth.StringWidth(head), head)
	}
	first := strings.Split(tui.Sprint(s.main().Item(3), tui.WithWidth(158), tui.WithColor(false)), "\n")[0]
	if w := runewidth.StringWidth(strings.TrimRight(first, " ")); w <= 100 {
		t.Fatalf("reply line is %d columns: %q", w, first)
	}
	native, _ := nativeBlockText(s.main().tr.Blocks[1], 160, false)
	if w := runewidth.StringWidth(strings.Split(native, "\n")[0]); w != 160 {
		t.Fatalf("native row is %d columns: %q", w, native)
	}
}

// One empty row always separates the transcript from the live UI under
// it, whatever that UI is, and only one.
func TestBlankRowAboveTheLiveUI(t *testing.T) {
	long := strings.Repeat("Line of assistant text.\n\n", 15) + "LAST LINE"
	for _, state := range []string{"streaming", "success", "approval", "question", "picker", "compacting", "scrolled"} {
		t.Run(state, func(t *testing.T) {
			fixture := state
			switch state {
			case "picker", "compacting", "scrolled":
				fixture = "success"
			}
			s := terminalVisualFixture(t, fixture, 80, 24)
			s.Emit(Block{Kind: KindAssistant, ID: "last", Text: long, Done: true})
			switch state {
			case "picker":
				s.promptActive = true
				s.picker = &pickerState{title: "Model", options: []string{"a", "b"}, reply: make(chan int, 1)}
			case "compacting":
				s.ctrl.mode = ModeRunning
				s.Emit(Block{ID: "c", Kind: KindCompaction, State: StateRunning, Text: "Compacting history..."})
				s.Emit(Block{Kind: KindAssistant, ID: "after", Text: long, Done: true})
			}
			s.drain()
			screen := tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24))
			if state == "scrolled" {
				s.main().vp.ScrollBy(-5)
				screen = tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24))
			}
			rows := strings.Split(screen.Text(), "\n")
			blank := func(y int) bool {
				if strings.TrimSpace(rows[y]) != "" {
					return false
				}
				// A dialog's or the prompt's band paints its padding rows.
				bg := screen.Cell(0, y).Style.Background
				return bg.Type == termtest.ColorDefault
			}
			if state == "scrolled" {
				// The mark sits on the transcript's bottom row, so the
				// live UI keeps its place: one empty row, then the UI.
				y := transcriptRow(t, screen.Text(), "more · Ctrl+End")
				if !blank(y+1) || blank(y+2) {
					t.Fatalf("want one empty row under the mark:\n%s", screen.Text())
				}
				return
			}
			last := -1
			for y, row := range rows {
				if strings.Contains(row, "LAST LINE") || strings.Contains(row, "Line of assistant text.") {
					last = y
				}
			}
			if last < 0 || last+2 >= len(rows) {
				t.Fatalf("no transcript text:\n%s", screen.Text())
			}
			if !blank(last + 1) {
				t.Fatalf("no empty row under the transcript:\n%s", screen.Text())
			}
			if blank(last + 2) {
				t.Fatalf("two empty rows under the transcript:\n%s", screen.Text())
			}
		})
	}
}

// The steering tip is UI of its own: an empty row separates it from the
// working line above it.
func TestBlankRowAboveTheSteeringTip(t *testing.T) {
	s := terminalVisualFixture(t, "streaming", 80, 24)
	s.drain()
	rows := strings.Split(tui.SprintScreen(s.View(), tui.WithWidth(80), tui.WithHeight(24)).Text(), "\n")
	working := transcriptRow(t, strings.Join(rows, "\n"), "Working")
	tip := transcriptRow(t, strings.Join(rows, "\n"), "Type to steer")
	if tip != working+2 || strings.TrimSpace(rows[working+1]) != "" {
		t.Fatalf("want one empty row between the working line and the tip:\n%s", strings.Join(rows, "\n"))
	}
}
