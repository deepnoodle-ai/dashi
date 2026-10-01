package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
	"time"
)

type mcpStartupUI interface {
	ApproveMCP(context.Context, mcpStartupRequest) (bool, error)
}

func (s *screen) ApproveMCP(ctx context.Context, r mcpStartupRequest) (bool, error) {
	id := s.promptSeq.Add(1)
	p := &pickerState{title: "Start MCP server " + workspace.PrintableText(r.Name) + "?", options: []string{"Start for this Dashi run", "Do not start"}, index: 1, reply: make(chan int, 1), startupReview: detailLines(strings.Split(strings.TrimSpace(r.Review), "\n"))}
	s.post(promptEvent{at: time.Now(), id: id, picker: p})
	select {
	case index := <-p.reply:
		return index == 0, ctx.Err()
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now(), id: id})
		return false, ctx.Err()
	}

}
func (u *lineUI) ApproveMCP(ctx context.Context, r mcpStartupRequest) (bool, error) {
	u.mu.Lock()
	fmt.Fprintln(u.out, workspace.PrintableText(r.Review))
	fmt.Fprintln(u.out, "1. Start for this Dashi run\n2. Do not start\nChoose 1 or 2: ")
	u.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case line := <-u.ctrl.Answers():
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "1", "y", "yes":
				return true, nil
			case "2", "n", "no":
				return false, nil
			}
			u.mu.Lock()
			fmt.Fprintln(u.out, "Choose 1 to start for this run, or 2 to decline.")
			u.mu.Unlock()
			u.ctrl.answerPending.Store(false)
		}
	}
}
func (a *app) initializeMCP(ctx context.Context, interactive bool) error {
	m := a.eng.cfg.MCP
	if m == nil {
		return a.eng.recoverTasks(ctx)
	}
	var approve mcpStartupFunc
	if interactive {
		if ui, ok := a.ctrl.ui.(mcpStartupUI); ok {
			approve = func(ctx context.Context, r mcpStartupRequest) (bool, error) {
				previous := a.ctrl.Mode()
				a.ctrl.setMode(ModeApproval)
				defer a.ctrl.setMode(previous)
				return ui.ApproveMCP(ctx, r)
			}
		}
	}
	err := m.Initialize(ctx, approve)
	if err == nil {
		a.eng.notice("%s", m.StartupSummary())
		if err := a.eng.recoverTasks(ctx); err != nil {
			a.eng.warn("Task recovery incomplete: %v", err)
		}
		a.ctrl.scheduleTaskNotices()
	}
	return err
}
func (m *mcpRuntime) StartupSummary() string {
	m.mu.Lock()
	set := m.set
	m.mu.Unlock()
	connected, failed, blocked, other := 0, 0, 0, 0
	if set != nil {
		for _, status := range set.Status() {
			switch status.State {
			case "connected":
				connected++
			case "failed":
				failed++
			case "declined":
				blocked++
			default:
				other++
			}
		}
	}
	for _, entry := range m.selection.entries {
		switch entry.state {
		case "failed":
			failed++
		case "unsupported", "disabled":
			other++
		}
	}
	summary := fmt.Sprintf("MCP: %d connected, %d failed, %d blocked, %d disabled/unsupported. Use /mcp for tools and sources.", connected, failed, blocked, other)
	for _, warning := range m.selection.warnings {
		summary += "\n" + warning
	}
	return summary
}

// startMCP runs after the UI can service dialogs. Until it completes, input
// cannot start turns, continuations, or queued prompts.
func (a *app) startMCP(interactive bool) {
	c := a.ctrl
	m := a.eng.cfg.MCP
	if m == nil || len(m.selection.entries) == 0 && len(m.selection.warnings) == 0 {
		// Empty MCP startup does not own the foreground or add transcript rows.
		// The recovery mutex makes the first prompt wait for this same snapshot.
		c.goWork(func() {
			if err := a.eng.recoverTasks(c.base); err != nil {
				c.notice("Task recovery incomplete: %v", err)
			}
			c.scheduleTaskNotices()
		})
		return
	}

	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.workerMode, c.mode, c.cancel = ModeCommand, ModeCommand, cancel
	c.mu.Unlock()
	c.goWork(func() {
		err := a.initializeMCP(ctx, interactive)
		cancel()
		c.mu.Lock()
		c.workerMode, c.cancel = ModeIdle, nil
		if c.mode != ModeApproval {
			c.mode = ModeIdle
		}
		quit := c.quit
		c.quit, c.quitAction = false, ""
		c.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			c.emit(warnBlock("MCP initialization failed; no work was started."))
		}
		if err != nil || quit {
			c.broker.cancelActive()
			c.ui.Quit()
		} else {
			c.scheduleTaskNotices()
		}
	})
}
func (c *Controller) mcpInput(line string) bool {
	// Approval replies must go to their dialog, never to command dispatch.
	if c.Mode() == ModeApproval {
		return false
	}
	name, arg := splitCommand(line)
	if name == "/status" {
		return false
	}
	if name == "/mcp" {
		c.mcpCommand(arg)
		return true
	}
	m := c.eng.cfg.MCP
	if m == nil || m.Ready() {
		return false
	}
	if name == "/quit" || name == "/exit" || name == "/q" {
		c.QuitDuringRun(line)
		return true
	}
	c.notice("MCP startup is in progress. Submit this input again after startup finishes.")
	return true
}
func (c *Controller) mcpInventory() {
	c.emit(Block{Kind: KindNotice, Text: c.eng.cfg.MCP.Inventory(), Done: true})
}

func (c *Controller) mcpCommand(arg string) {
	if arg != "" {
		c.notice("Usage: /mcp")
		return
	}
	c.mcpInventory()
}

// The review viewport keeps all configuration details inspectable while the
// two startup-only choices remain visible on an 80x24 terminal.
func mcpStartupView(p *pickerState) tui.View {
	views := []tui.View{tui.Divider(), tui.MaxHeight(2, tui.Text(" %s", p.title).Wrap().Bold()), tui.Height(8, tui.Viewport(&p.startupVP, p.startupReview).Gap(0)), tui.Text(" Page Up/Down inspect · ↑/↓ choose · Enter confirms · Esc declines").Wrap().Style(hintStyle)}
	for index, option := range p.options {
		prefix := "   "
		if index == p.index {
			prefix = " ❯ "
		}
		view := tui.Text("%s%s", prefix, option)
		if index == p.index {
			view = view.Fg(tui.ColorCyan).Bold()
		}
		views = append(views, view)
	}
	return tui.Stack(views...).Gap(0)
}
