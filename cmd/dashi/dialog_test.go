package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

// renderAt draws the screen at width by height and returns its rows.
func (h *screenHarness) renderAt(width, height int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scr.width, h.scr.height = width, height
	h.scr.drain()
	text := tui.SprintScreen(h.scr.View(), tui.WithWidth(width), tui.WithHeight(height)).Text()
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

func TestTaskQuestionShowsTaskOrigin(t *testing.T) {
	h := newScreenHarness(t, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.scr.Ask(t.Context(), taskQuestion{Question: Question{Prompt: "Which branch?", Choices: []workspace.Choice{{Label: "main"}}}, Task: "t_abc123"})
	}()
	h.waitFor(func() bool { return h.scr.asking != nil })
	screen := strings.Join(h.renderAt(80, 24), "\n")
	h.mu.Lock()
	h.scr.finishQuestion(questionAnswer{choices: []int{0}})
	h.mu.Unlock()
	<-done
	for _, want := range []string{"Question pending", "From task t_abc123 · /task t_abc123 shows it", "   Which branch?", "/ commands · Esc interrupts"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("missing %q in:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "Turn") || strings.Contains(screen, "/task-stop") {
		t.Fatalf("internal IDs or long command list shown:\n%s", screen)
	}
}

func TestApprovalFitsSmallTerminalAndDenies(t *testing.T) {
	h := newScreenHarness(t, 0)
	prompt := strings.Repeat("Wait for the parent agent's message before finishing.\n", 30)
	answers := make(chan Approval, 1)
	go func() {
		a, _ := h.scr.Approve(t.Context(), toolApproval{ApprovalRequest: ApprovalRequest{
			Tool: "task", Summary: "task", RememberScope: "This tool with these exact arguments",
			Arguments: `{"subagent":"explore","description":"messaging test wait","prompt":` + jsonString(prompt) + `}`,
		}})
		answers <- a
	}()
	h.waitFor(func() bool { return h.scr.approval != nil })

	rows := h.renderAt(80, 24)
	if len(rows) != 24 {
		t.Fatalf("rendered %d rows", len(rows))
	}
	screen := strings.Join(rows, "\n")
	if !strings.Contains(screen, "Allow task?  explore subagent: messaging test wait") ||
		!strings.Contains(rows[20], "y Allow once") || !strings.Contains(rows[20], "n Deny") ||
		!strings.Contains(rows[22], "e shows all") || !strings.Contains(rows[23], "fixture") {
		t.Fatalf("layout at 80x24:\n%s", screen)
	}
	if strings.Contains(screen, "Turn") || strings.Contains(screen, "call_") || strings.Contains(screen, "{") || strings.Contains(screen, `\n`) {
		t.Fatalf("internal IDs or raw JSON shown:\n%s", screen)
	}

	// e shows every line; the details scroll and the choices stay put.
	h.key(tui.KeyEvent{Rune: 'e'})
	h.key(tui.KeyEvent{Key: tui.KeyEnd})
	rows = h.renderAt(80, 24)
	if !strings.Contains(rows[20], "y Allow once") || !strings.Contains(rows[22], "e collapses") ||
		!strings.Contains(strings.Join(rows, "\n"), "covers this tool with these exact arguments") {
		t.Fatalf("expanded at 80x24:\n%s", strings.Join(rows, "\n"))
	}

	// n asks for a reason in place of the choices, and Enter denies.
	h.key(tui.KeyEvent{Rune: 'n'})
	rows = h.renderAt(80, 24)
	if !h.check(func() bool { return h.scr.approval.denying }) || !strings.Contains(rows[20], "Reason:") || strings.Contains(rows[20], "Allow once") ||
		!strings.Contains(rows[22], "Deny: Enter submits") {
		t.Fatalf("deny at 80x24:\n%s", strings.Join(rows, "\n"))
	}
	h.key(tui.KeyEvent{Rune: 'y'})
	if !h.check(func() bool { return h.scr.approval != nil }) {
		t.Fatal("y answered while a reason was being typed")
	}
	h.mu.Lock()
	h.scr.denialSubmit(h.scr.promptID)("Use the explore results instead")
	h.mu.Unlock()
	if a := <-answers; a.Allow || a.Reason != "Use the explore results instead" {
		t.Fatalf("answer=%+v", a)
	}
}
