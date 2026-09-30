package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
)

const instructionLimit = 64 << 10

type instructionSource struct {
	Path   string
	Target string
	Status string
}
type selectedSkillSource struct {
	Info skill.Info
	Root string
}
type discoveredResources struct {
	Instructions       dashi.ContextItem
	InstructionSource  dashi.ContextItem
	InstructionSources []instructionSource
	Skills             []selectedSkillSource
	Set                *skill.Set
	Bundle             *skill.Bundle
	Candidates         []skill.Info
	Warnings           []skill.Warning
}

// Discovery selects contextual content only; it grants no tools or file access.
func discoverResources(workspace, home string) (*discoveredResources, error) {
	selected := &discoveredResources{}
	var warnings []skill.Warning
	selected.Instructions, selected.InstructionSources, warnings = discoverInstructions(workspace)
	sourceText := "No workspace instruction source selected."
	for _, source := range selected.InstructionSources {
		if source.Status == "loaded" {
			sourceText = "Workspace instructions from " + source.Path + ":"
		}
	}
	selected.InstructionSource = instructionItem(sourceText)
	selected.InstructionSource.Name = "workspace-instructions-source"
	selected.InstructionSource.Message.Content[0].Reminder.Name = "workspace-instructions-source"
	selected.Warnings = append(selected.Warnings, warnings...)
	set, warnings := skill.LoadDirs(skill.DefaultDirs(workspace, home)...)
	selected.Set, selected.Candidates = set, set.Candidates()
	selected.Warnings = append(selected.Warnings, warnings...)
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		return nil, fmt.Errorf("build skill catalog: %w", err)
	}
	selected.Bundle = bundle
	for _, info := range set.Skills() {
		source := selectedSkillSource{Info: info}
		if info.Dir != "" {
			target, err := filepath.EvalSymlinks(info.Dir)
			if err == nil {
				target, err = filepath.Abs(target)
			}
			if err == nil {
				var stat os.FileInfo
				stat, err = os.Stat(target)
				if err == nil && !stat.IsDir() {
					err = errors.New("selected skill root is not a directory")
				}
			}
			if err != nil {
				selected.Warnings = append(selected.Warnings, skill.Warning{Path: info.Dir, Message: "supporting files unavailable: " + err.Error()})
			} else {
				source.Root = target
			}
		}
		selected.Skills = append(selected.Skills, source)
	}
	return selected, nil
}
func discoverInstructions(workspace string) (dashi.ContextItem, []instructionSource, []skill.Warning) {
	empty := instructionItem("No workspace instructions selected.")
	sources := []instructionSource{{Path: filepath.Join(workspace, "AGENTS.md"), Status: "missing"}, {Path: filepath.Join(workspace, "CLAUDE.md"), Status: "unselected"}}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		sources[0].Status = "error"
		return empty, sources, []skill.Warning{{Path: workspace, Message: "cannot inspect workspace instructions: " + err.Error()}}
	}
	defer root.Close()
	index := 0
	if _, err = root.Lstat("AGENTS.md"); errors.Is(err, os.ErrNotExist) {
		index = 1
		sources[1].Status = "missing"
	} else if err != nil {
		sources[0].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[0].Path, Message: "cannot inspect: " + err.Error()}}
	}
	name := filepath.Base(sources[index].Path)
	if _, err = root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return empty, sources, nil
	} else if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "cannot inspect: " + err.Error()}}
	}
	before, err := root.Stat(name)
	if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "instructions omitted: " + err.Error()}}
	}
	data, err := readBoundedResource(root, name, instructionLimit, before)
	if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "instructions omitted: " + err.Error()}}
	}
	target, err := filepath.EvalSymlinks(sources[index].Path)
	if err == nil {
		var relative string
		canonicalWorkspace, resolveErr := filepath.EvalSymlinks(workspace)
		err = resolveErr
		if err == nil {
			relative, err = filepath.Rel(canonicalWorkspace, target)
		}
		if err == nil && (filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			err = errors.New("instruction target escapes workspace")
		}
		if err == nil {
			var after os.FileInfo
			after, err = root.Stat(relative)
			if err == nil && !os.SameFile(before, after) {
				err = errors.New("instruction target changed")
			}
		}
	}
	if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "source changed: " + err.Error()}}
	}
	sources[index].Status = "loaded"
	sources[index].Target = target
	text := string(data)
	if text == "" {
		text = "Workspace instruction file is empty."
	}
	item := instructionItem(text)
	sum := sha256.Sum256(append([]byte(sources[index].Path+"\x00"), data...))
	item.ID = "workspace-instructions-" + hex.EncodeToString(sum[:])
	return item, sources, nil
}
func instructionItem(text string) dashi.ContextItem {
	sum := sha256.Sum256([]byte(text))
	return dashi.ContextItem{ID: "workspace-instructions-" + hex.EncodeToString(sum[:]), Name: "workspace-instructions", Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "workspace-instructions", Tier: llm.ReminderContextual, Content: text})}
}
func readBoundedResource(root *os.Root, path string, limit int, approved ...os.FileInfo) ([]byte, error) {
	before, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if len(approved) > 0 {
		if !os.SameFile(before, approved[0]) {
			return nil, errors.New("resource changed after approval")
		}
		before = approved[0]
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	file, err := openResourceRead(root, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, current) {
		return nil, errors.New("resource changed while opening")
	}
	if current.Size() > int64(limit) {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("file is not valid UTF-8")
	}
	return data, nil
}
