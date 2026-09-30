package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

type skillFileArgs struct {
	Skill string `json:"skill" description:"Name of a selected folder skill"`
	Path  string `json:"path" description:"Supporting file path relative to the skill base directory"`
}
type validatedSkillFile struct {
	Args               skillFileArgs
	Root               string
	RootInfo, FileInfo os.FileInfo
}

func resolveSkillFile(binding *resourceBinding, args skillFileArgs) (*validatedSkillFile, error) {
	var selected *selectedSkillSource
	for i := range binding.Snapshot.Skills {
		if strings.EqualFold(strings.TrimSpace(args.Skill), binding.Snapshot.Skills[i].Info.Name) {
			selected = &binding.Snapshot.Skills[i]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("skill %q is not selected", args.Skill)
	}
	if selected.Info.Dir == "" || selected.Root == "" {
		return nil, fmt.Errorf("skill %q has no supporting-file folder", selected.Info.Name)
	}
	if args.Path == "" || strings.ContainsRune(args.Path, 0) {
		return nil, errors.New("supporting file path is required")
	}
	for _, part := range strings.Split(filepath.ToSlash(args.Path), "/") {
		if part == ".." {
			return nil, errors.New("supporting file traversal is not allowed")
		}
	}
	currentRoot, err := filepath.EvalSymlinks(selected.Info.Dir)
	if err != nil {
		return nil, err
	}
	currentRoot, err = filepath.Abs(currentRoot)
	if err != nil {
		return nil, err
	}
	if currentRoot != selected.Root {
		return nil, errors.New("selected skill folder changed; submit a new prompt to refresh resources")
	}
	path := args.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(selected.Root, path)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(selected.Root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errors.New("supporting file escapes the selected skill folder")
	}
	before, err := os.Stat(selected.Root)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(selected.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	current, err := root.Stat(".")
	if err != nil || !os.SameFile(before, current) {
		return nil, errors.Join(errors.New("skill folder changed while opening"), err)
	}
	info, err := root.Stat(relative)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("supporting file is not a regular file")
	}
	if info.Size() > instructionLimit {
		return nil, fmt.Errorf("supporting file exceeds %d bytes", instructionLimit)
	}
	return &validatedSkillFile{Args: skillFileArgs{selected.Info.Name, target}, Root: selected.Root, RootInfo: current, FileInfo: info}, nil
}
func (m *resourceBindingManager) validateSkillFile(_ context.Context, plan dashi.ToolPlan, binding *resourceBinding) (dashi.ToolDecision, error) {
	var args skillFileArgs
	if err := json.Unmarshal(plan.Call.Args, &args); err != nil {
		return dashi.Deny("Invalid supporting-file arguments."), nil
	}
	selected, err := resolveSkillFile(binding, args)
	if err != nil {
		return dashi.Deny("Cannot read supporting file: " + err.Error()), nil
	}
	ref := dashi.CallRef{TurnID: plan.Turn.ID, CallID: plan.Call.ID}
	m.mu.Lock()
	previous := m.files[ref]
	if previous != nil && (previous.Args != selected.Args || !os.SameFile(previous.RootInfo, selected.RootInfo) || !os.SameFile(previous.FileInfo, selected.FileInfo)) {
		m.mu.Unlock()
		return dashi.Deny("Supporting file changed during validation; request it again."), nil
	}
	m.files[ref] = selected
	m.mu.Unlock()
	raw, err := json.Marshal(selected.Args)
	if err != nil {
		return dashi.Deny("Cannot normalize supporting-file arguments."), nil
	}
	if !bytes.Equal(raw, plan.Call.Args) {
		return dashi.Rewrite(json.RawMessage(raw))
	}
	return dashi.ToolDecision{}, nil
}
func (m *resourceBindingManager) skillFileTool() (tool.Tool, error) {
	return tool.Func("read_skill_file", "Read a UTF-8 supporting file of a selected folder skill (at most 64 KiB). Use this for supporting files outside the workspace; ordinary workspace files use read_file. The selected folder grants no write or shell authority.", func(ctx context.Context, call tool.Call, args skillFileArgs) (tool.Outcome, error) {
		ref, ok := dashi.CurrentCall(ctx)
		if !ok {
			return tool.Outcome{}, tool.Fatal(errors.New("supporting-file reader requires a recorded turn"))
		}
		binding, _, err := m.bind(ctx, ref.TurnID, false)
		if err != nil {
			return tool.Outcome{}, tool.Fatal(fmt.Errorf("resource recovery: %w", err))
		}
		if binding.Legacy {
			return tool.Failed("Supporting files were not selected for this turn."), nil
		}
		m.mu.Lock()
		selected := m.files[ref]
		m.mu.Unlock()
		// An acknowledged per-call decision can survive a process restart. Rebuild
		// its canonical target, while remembered process grants remain absent.
		if selected == nil {
			selected, err = resolveSkillFile(binding, args)
			if err != nil {
				return tool.Failed("Cannot read supporting file: " + err.Error()), nil
			}
		}
		if args != selected.Args || !filepath.IsAbs(args.Path) {
			return tool.Failed("Supporting file arguments were not validated."), nil
		}
		root, err := os.OpenRoot(selected.Root)
		if err != nil {
			return tool.Failed("Cannot open skill folder: " + err.Error()), nil
		}
		defer root.Close()
		current, err := root.Stat(".")
		if err != nil || !os.SameFile(current, selected.RootInfo) {
			return tool.Failed("Skill folder changed after approval."), nil
		}
		relative, err := filepath.Rel(selected.Root, args.Path)
		if err != nil {
			return tool.Failed("Cannot locate supporting file: " + err.Error()), nil
		}
		before, err := root.Stat(relative)
		if err != nil || !os.SameFile(before, selected.FileInfo) {
			return tool.Failed("Supporting file changed after approval."), nil
		}
		// readBoundedResource opens under this root and checks the opened identity
		// against its pre-open stat. Verify the approved identity again afterward.
		data, err := readBoundedResource(root, relative, instructionLimit, selected.FileInfo)
		if err != nil {
			return tool.Failed("Cannot read supporting file: " + err.Error()), nil
		}
		after, err := root.Stat(relative)
		if err != nil || !os.SameFile(after, selected.FileInfo) {
			return tool.Failed("Supporting file changed while reading."), nil
		}
		return tool.Succeeded(fmt.Sprintf("Skill %s — %s\n\n%s", args.Skill, args.Path, data)), nil
	}, tool.ReadOnly(), tool.Idempotent(), tool.WithStrictArguments())
}
