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
	last := c.eng.Last(ctx)
	if last != nil && last.Open() {
		fmt.Fprintln(&out, "Current turn resources (frozen):")
		sess, loader := manager.environment()
		binding, err := manager.preflight(ctx, sess, loader, last.View.ID)
		switch {
		case err != nil:
			fmt.Fprintf(&out, "  Unavailable: %v\n", err)
		case binding == nil:
			fmt.Fprintln(&out, "  This turn has no recorded resource pin.")
		case binding.Legacy:
			fmt.Fprintln(&out, "  This earlier turn has no CLI resource selection.")
		default:
			fmt.Fprintf(&out, "  Catalog %s; %d selected skills\n", binding.Reference.CatalogDigest, len(binding.Snapshot.Skills))
			fmt.Fprintln(&out, "  Instructions are contextual; they grant no tool authority.")
			for _, source := range binding.Snapshot.InstructionSources {
				if source.Status == "loaded" {
					fmt.Fprintf(&out, "  Instructions: %s\n", source.Path)
				}
			}
			for _, source := range binding.Snapshot.Skills {
				fmt.Fprintf(&out, "  Skill %s (user %t, model %t); source %s\n", source.Info.Name, source.Info.UserInvocable, source.Info.ModelInvocable, source.Info.Path)
				writeSupportingScope(&out, source)
			}
		}
		fmt.Fprintln(&out, "Discovered resources for the next turn (not applied):")
	} else {
		fmt.Fprintln(&out, "Discovered resources for the next turn:")
	}
	for _, source := range found.Snapshot.InstructionSources {
		fmt.Fprintf(&out, "  %s: %s\n", source.Path, source.Status)
		if source.Target != "" && source.Target != source.Path {
			fmt.Fprintf(&out, "    Target: %s\n", source.Target)
		}
	}
	fmt.Fprintln(&out, "  Instructions are contextual; they grant no tool authority.")
	fmt.Fprintf(&out, "Skills: %d selected, %d shadowed\n", len(found.Snapshot.Skills), len(found.Candidates)-len(found.Snapshot.Skills))
	seen := map[string]bool{}
	for _, info := range found.Candidates {
		key := strings.ToLower(info.Name)
		status := "selected"
		if seen[key] {
			status = "shadowed"
		}
		seen[key] = true
		fmt.Fprintf(&out, "  /%s (%s; user %t, model %t) — %s\n", info.Name, status, info.UserInvocable, info.ModelInvocable, resourceTier(manager.workspace, manager.home, info.Path))
		if builtinCommand(info.Name) {
			fmt.Fprintln(&out, "    Built-in command wins; custom slash invocation is unavailable.")
		}
		fmt.Fprintf(&out, "    Source: %s\n", info.Path)
		if info.ArgumentHint != "" {
			fmt.Fprintf(&out, "    Arguments: %s\n", info.ArgumentHint)
		}
		for _, source := range found.Snapshot.Skills {
			if source.Info.Path == info.Path {
				writeSupportingScope(&out, source)
			}
		}
	}
	if len(found.Warnings) > 0 {
		fmt.Fprintln(&out, "Warnings:")
		for _, warning := range found.Warnings {
			fmt.Fprintf(&out, "  %s: %s\n", warning.Path, warning.Message)
		}
	}
	fmt.Fprintln(&out, "Registered tools:")
	tools, err := c.eng.cfg.Wiring.Tools.Tools(ctx)
	if err != nil {
		fmt.Fprintf(&out, "  Unavailable: %v\n", err)
	} else {
		slices.SortFunc(tools, func(a, b tool.Tool) int { return strings.Compare(a.Definition.Name, b.Definition.Name) })
		for _, entry := range tools {
			scope := c.eng.cfg.Wiring.Scopes[entry.Definition.Name]
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
		fmt.Fprintln(out, "    Approval: each call; this run may remember exact canonical arguments only.")
	} else {
		fmt.Fprintln(out, "    Supporting reads: unavailable (no selected folder).")
	}
}

func builtinCommand(name string) bool {
	switch strings.ToLower(name) {
	case "copy", "scrollback", "mouse", "compact", "model", "settings", "effort", "thinking", "temperature", "max-tokens", "new", "sessions", "continue", "queue", "done", "discard", "quit", "exit", "help", "resources":
		return true
	}
	return false
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
		return c.eng.promptWithResources(ctx, text, found)
	}
	if running {
		c.goWork(func() { _, _ = invoke(c.base) })
		return
	}
	c.startRun(invoke)
}
func (m *resourceBindingManager) approvalRequest(ctx context.Context, request ApprovalRequest) ApprovalRequest {
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
	binding := m.bound[view.ID]
	m.mu.Unlock()
	if binding == nil {
		return request
	}
	for _, source := range binding.Snapshot.Skills {
		if source.Info.Name == args.Skill {
			request.Summary = fmt.Sprintf("Read supporting file for skill %s (source %s): %s", args.Skill, source.Info.Path, args.Path)
			break
		}
	}
	return request
}
func resourceStartupSummary(found *discoveredResources) string {
	instruction := "none"
	for _, source := range found.Snapshot.InstructionSources {
		if source.Status == "loaded" {
			instruction = source.Path
		}
	}
	return fmt.Sprintf("Instructions: %s; %d selected skills (%d model, %d shadowed). Use /resources for sources and access scopes.", instruction, len(found.Snapshot.Skills), len(found.Snapshot.Definitions), len(found.Candidates)-len(found.Snapshot.Skills))
}
