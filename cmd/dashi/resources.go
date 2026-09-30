package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Path   string `json:"path"`
	Target string `json:"target,omitempty"`
	Status string `json:"status"`
}
type selectedSkillSource struct {
	Info skill.Info `json:"info"`
	Root string     `json:"root,omitempty"`
}
type resourceSnapshot struct {
	Version            int                   `json:"version"`
	Instructions       dashi.ContextItem     `json:"instructions"`
	InstructionSources []instructionSource   `json:"instruction_sources"`
	Definitions        []skill.Definition    `json:"definitions"`
	Skills             []selectedSkillSource `json:"skills"`
	CatalogDigest      string                `json:"catalog_digest"`
}
type discoveredResources struct {
	Snapshot   resourceSnapshot
	Set        *skill.Set
	Bundle     *skill.Bundle
	Candidates []skill.Info
	Warnings   []skill.Warning
}

// Discovery selects contextual content only; it grants no tools or file access.
func discoverResources(workspace, home string) (*discoveredResources, error) {
	selected := &discoveredResources{}
	selected.Snapshot.Version = 1
	var warnings []skill.Warning
	selected.Snapshot.Instructions, selected.Snapshot.InstructionSources, warnings = discoverInstructions(workspace)
	selected.Warnings = append(selected.Warnings, warnings...)
	set, warnings := skill.LoadDirs(skill.DefaultDirs(workspace, home)...)
	selected.Set, selected.Candidates = set, set.Candidates()
	selected.Warnings = append(selected.Warnings, warnings...)
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		return nil, fmt.Errorf("build skill catalog: %w", err)
	}
	selected.Bundle = bundle
	selected.Snapshot.Definitions = bundle.Definitions()
	selected.Snapshot.CatalogDigest = bundle.Digest()
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
		selected.Snapshot.Skills = append(selected.Snapshot.Skills, source)
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
	data, err := readBoundedResource(root, name, instructionLimit)
	if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "instructions omitted: " + err.Error()}}
	}
	target, err := filepath.EvalSymlinks(sources[index].Path)
	if err != nil {
		sources[index].Status = "error"
		return empty, sources, []skill.Warning{{Path: sources[index].Path, Message: "source changed: " + err.Error()}}
	}
	sources[index].Status = "loaded"
	sources[index].Target = target
	item := instructionItem("Workspace instructions from " + sources[index].Path + ":")
	if len(data) > 0 {
		item.Message.Content = append(item.Message.Content, llm.ReminderMessage(llm.Reminder{Name: "workspace-instructions", Tier: llm.ReminderContextual, Content: string(data)}).Content...)
	}
	sum := sha256.Sum256(append([]byte(sources[index].Path+"\x00"), data...))
	item.ID = "workspace-instructions-" + hex.EncodeToString(sum[:])
	return item, sources, nil
}
func instructionItem(text string) dashi.ContextItem {
	sum := sha256.Sum256([]byte(text))
	return dashi.ContextItem{ID: "workspace-instructions-" + hex.EncodeToString(sum[:]), Name: "workspace-instructions", Origin: dashi.ContextRuntime, Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "workspace-instructions", Tier: llm.ReminderContextual, Content: text})}
}
func readBoundedResource(root *os.Root, path string, limit int) ([]byte, error) {
	before, err := root.Stat(path)
	if err != nil {
		return nil, err
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
func (s resourceSnapshot) encode() ([]byte, error) {
	if s.Version != 1 {
		return nil, errors.New("unsupported resource snapshot version")
	}
	bundle, err := skill.New(s.Definitions, skill.Options{})
	if err != nil {
		return nil, err
	}
	if bundle.Digest() != s.CatalogDigest {
		return nil, errors.New("resource catalog digest mismatch")
	}
	if err := s.validateContent(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}
func (s resourceSnapshot) validateContent() error {
	item := s.Instructions
	if item.ID == "" || item.Name != "workspace-instructions" || item.Origin != dashi.ContextRuntime || !item.Snapshot || item.NextCallOnly || item.Message.Role != llm.RoleUser || item.Message.Source != nil || item.Message.Context != nil || len(item.Message.Content) < 1 || len(item.Message.Content) > 2 {
		return errors.New("invalid contextual workspace instructions")
	}
	for _, content := range item.Message.Content {
		if content.Kind != llm.ContentReminder || content.Reminder == nil || content.Reminder.Name != item.Name || content.Reminder.Tier != llm.ReminderContextual || content.Reminder.From != "" {
			return errors.New("invalid contextual workspace instructions")
		}
		if err := content.Reminder.Validate(); err != nil {
			return err
		}
	}
	selected := map[string]selectedSkillSource{}
	for _, source := range s.Skills {
		key := strings.ToLower(source.Info.Name)
		if key == "" {
			return errors.New("empty selected skill name")
		}
		if _, duplicate := selected[key]; duplicate {
			return errors.New("duplicate selected skill metadata")
		}
		if !filepath.IsAbs(source.Info.Path) || (source.Info.Dir != "" && !filepath.IsAbs(source.Info.Dir)) || (source.Root != "" && (!filepath.IsAbs(source.Root) || filepath.Clean(source.Root) != source.Root || source.Info.Dir == "")) {
			return errors.New("invalid selected skill source")
		}
		selected[key] = source
	}
	for _, definition := range s.Definitions {
		source, ok := selected[strings.ToLower(definition.Name)]
		if !ok || !source.Info.ModelInvocable || source.Info.Name != definition.Name || source.Info.Dir != definition.Dir || source.Info.Description != definition.Description {
			return errors.New("resource definition does not match selected source")
		}
	}
	return nil
}
func decodeResourceSnapshot(raw []byte) (resourceSnapshot, *skill.Bundle, error) {
	var snapshot resourceSnapshot
	if !utf8.Valid(raw) {
		return snapshot, nil, errors.New("invalid UTF-8 resource snapshot")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return snapshot, nil, errors.New("trailing resource snapshot JSON")
	}
	if _, err := snapshot.encode(); err != nil {
		return snapshot, nil, err
	}

	bundle, err := skill.New(snapshot.Definitions, skill.Options{})
	return snapshot, bundle, err
}
