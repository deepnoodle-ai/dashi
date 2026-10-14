package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

func (c *Controller) resources(ctx context.Context) {
	manager := c.eng.cfg.Resources
	if manager == nil {
		c.notice("Resource discovery is unavailable.")
		return
	}
	noticeID := "resources:" + dashi.NewID()
	c.emit(Block{ID: noticeID, Kind: KindNotice, Text: "Loading resources..."})
	found, err := manager.discover()
	if err != nil {
		c.emit(Block{ID: noticeID, Kind: KindNotice, Text: fmt.Sprintf("Resources cannot be inspected: %v.", err), Done: true})
		return
	}
	var out strings.Builder
	fmt.Fprintln(&out, "Available instructions and skills:")
	for _, source := range found.InstructionSources {
		fmt.Fprintf(&out, "  %s: %s\n", source.Path, source.Status)
		if source.Target != "" && source.Target != source.Path {
			fmt.Fprintf(&out, "    Target: %s\n", source.Target)
		}
	}
	fmt.Fprintln(&out, "  Instructions are contextual; they grant no tool authority.")
	fmt.Fprintf(&out, "Skills: %d selected, %d shadowed\n", len(found.Skills), len(found.Candidates)-len(found.Skills))
	seen := map[string]bool{}
	builtins := c.builtinCommands()
	for _, info := range found.Candidates {
		key := strings.ToLower(info.Name)
		status := "selected"
		if seen[key] {
			status = "shadowed"
		}
		seen[key] = true
		fmt.Fprintf(&out, "  /%s (%s; user %t, model %t) — %s\n", info.Name, status, info.UserInvocable, info.ModelInvocable, resourceTier(manager.workspace, manager.home, info.Path))
		if namesCommand(builtins, "/"+info.Name) {
			fmt.Fprintln(&out, "    Built-in command wins; custom slash invocation is unavailable.")
		}
		fmt.Fprintf(&out, "    Source: %s\n", info.Path)
		if info.ArgumentHint != "" {
			fmt.Fprintf(&out, "    Arguments: %s\n", info.ArgumentHint)
		}
		for _, source := range found.Skills {
			if source.Info.Path == info.Path {
				writeSupportingScope(&out, source)
			}
		}
	}
	for _, section := range []struct {
		heading string
		skipped bool
	}{{"Not loaded:", true}, {"Loaded with adjustments:", false}} {
		heading := section.heading
		for _, warning := range found.Warnings {
			if warning.Skipped != section.skipped {
				continue
			}
			if heading != "" {
				fmt.Fprintln(&out, heading)
				heading = ""
			}
			fmt.Fprintf(&out, "  %s: %s\n", warning.Path, warning.Message)
		}
	}
	fmt.Fprintln(&out, "Remembered supporting-file approvals for this Dashi run:")
	remembered := manager.rememberedApprovals()
	if len(remembered) == 0 {
		fmt.Fprintln(&out, "  None.")
	}
	for _, args := range remembered {
		fmt.Fprintf(&out, "  Skill %s: %s\n", args.Skill, args.Path)
	}
	fmt.Fprintln(&out, "  Exact read_skill_file arguments only; selected folder checks still apply.")
	if c.eng.cfg.MCP != nil {
		fmt.Fprintln(&out, c.eng.cfg.MCP.Inventory())
	}
	fmt.Fprintln(&out, "Registered tools:")
	tools, err := c.eng.cfg.Wiring.Tools.Tools(ctx)
	if err != nil {
		fmt.Fprintf(&out, "  Unavailable: %v\n", err)
	} else {
		slices.SortFunc(tools, func(a, b tool.Tool) int { return strings.Compare(a.Definition.Name, b.Definition.Name) })
		for _, entry := range tools {
			scope := c.eng.cfg.Wiring.Scopes[entry.Definition.Name]
			if strings.HasPrefix(entry.Definition.Name, "mcp__") {
				scope = "external MCP server; exact-call approval required; no workspace grant"
			}
			if scope == "" {
				scope = "registered host tool; workspace approval policy"
			}
			fmt.Fprintf(&out, "  %s — %s\n", entry.Definition.Name, scope)
		}
	}
	c.emit(Block{ID: noticeID, Kind: KindNotice, Text: strings.TrimSpace(out.String()), Done: true})
}

func writeSupportingScope(out *strings.Builder, source selectedSkillSource) {
	if source.Root != "" {
		fmt.Fprintf(out, "    Eligible supporting reads: %s; no write or shell grant\n", source.Root)
		fmt.Fprintln(out, "    Approval required unless these exact arguments are remembered below.")
	} else {
		fmt.Fprintln(out, "    Supporting reads: unavailable (no selected folder).")
	}
}

// builtinCommands returns the built-in commands this screen offers, without
// the skill commands. A skill whose name is one of their names or aliases
// cannot be invoked with a slash.
func (c *Controller) builtinCommands() []commandEntry {
	var builtins []commandEntry
	for _, entry := range c.commands() {
		if entry.group != skillGroup {
			builtins = append(builtins, entry)
		}
	}
	return builtins
}
func resourceTier(dir, home, path string) string {
	for _, level := range []struct{ base, label string }{{dir, "project"}, {home, "home"}} {
		for _, search := range skill.DefaultDirs(level.base, "") {
			relative, err := filepath.Rel(search, path)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
				suffix, _ := filepath.Rel(level.base, search)
				return level.label + " " + filepath.ToSlash(suffix)
			}
		}
	}
	return "configured source"
}
func (c *Controller) skillCommand(line string, running bool) {
	manager := c.eng.cfg.Resources
	if manager == nil {
		c.notice("Unknown command %s. Type /help.", strings.Fields(line)[0])
		return
	}
	invoke := func(ctx context.Context) (Result, error) {
		name, args, _ := strings.Cut(line, " ")
		name = strings.TrimPrefix(name, "/")
		found, err := manager.discover()
		if err != nil {
			c.notice("Skills cannot be refreshed: %v.", err)
			return Result{}, nil
		}
		var selected *skill.Info
		for _, info := range found.Set.Skills() {
			if strings.EqualFold(info.Name, name) {
				copy := info
				selected = &copy
				break
			}
		}
		if selected == nil {
			c.notice("Unknown command /%s. Type /help or /resources.", name)
			return Result{}, nil
		}
		if !selected.UserInvocable {
			c.notice("Skill /%s cannot be invoked as a user command.", selected.Name)
			return Result{}, nil
		}
		if running {
			c.notice("/%s must wait until the turn ends. Submit it again then.", selected.Name)
			return Result{}, nil
		}
		text, ok := found.Set.Expand(name, args)
		if !ok {
			return Result{}, fmt.Errorf("skill /%s could not be expanded", name)
		}
		return c.eng.Prompt(ctx, text)
	}
	if running {
		c.goWork(func() { _, _ = invoke(c.base) })
		return
	}
	c.startRun(invoke)
}
func (m *resourceManager) approvalRequest(ctx context.Context, request ApprovalRequest) ApprovalRequest {
	if request.Tool != "read_skill_file" {
		return request
	}
	view, ok := dashi.CurrentTurn(ctx)
	if !ok {
		return request
	}
	var args skillFileArgs
	if json.Unmarshal(request.Args, &args) != nil {
		return request
	}
	m.mu.Lock()
	for ref, file := range m.files {
		if ref.TurnID == view.ID && file.Args == args {
			request.Summary = fmt.Sprintf("Read supporting file for skill %s (source %s): %s", args.Skill, file.SourcePath, args.Path)
			break
		}
	}
	m.mu.Unlock()
	return request
}
