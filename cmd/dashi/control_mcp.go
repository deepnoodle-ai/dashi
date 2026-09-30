package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi/workspace"
)

type mcpStartupUI interface {
	ApproveMCP(context.Context, mcpStartupRequest) (bool, error)
}

func (s *screen) ApproveMCP(ctx context.Context, r mcpStartupRequest) (bool, error) {
	index, ok := s.ChooseDetailed(ctx, "Start MCP server "+workspace.PrintableText(r.Name)+"?", []string{"Start for this Dashi run", "Do not start"}, []string{r.Review, r.Review}, 1)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return ok && index == 0, nil
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
		return nil
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
// cannot admit turns, continuations, or queued prompts.
func (a *app) startMCP(interactive bool) {
	if a.eng.cfg.MCP == nil {
		return
	}
	c := a.ctrl
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.mode, c.cancel = ModeCommand, cancel
	c.mu.Unlock()
	c.goWork(func() {
		err := a.initializeMCP(ctx, interactive)
		cancel()
		c.mu.Lock()
		c.mode, c.cancel = ModeIdle, nil
		quit := c.quit
		c.quit, c.quitAction = false, ""
		c.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			c.emit(warnBlock("MCP initialization failed; no work was started."))
		}
		if err != nil || quit {
			c.ui.Quit()
		}
	})
}
func (c *Controller) mcpInput(line string) bool {
	// Approval replies must go to their dialog, never to command dispatch.
	if c.Mode() == ModeApproval {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(line), "/mcp") {
		c.mcpInventory()
		return true
	}
	m := c.eng.cfg.MCP
	if m == nil || m.Ready() {
		return false
	}
	if line == "/quit" || line == "/exit" {
		c.QuitDuringRun(line)
		return true
	}
	c.notice("MCP startup is in progress. Submit this input again after startup finishes.")
	return true
}
func (c *Controller) mcpInventory() {
	c.emit(Block{Kind: KindNotice, Text: c.eng.cfg.MCP.Inventory(), Done: true})
}
