package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// workspaceCompletion indexes names once outside rendering. It never opens contents.
type workspaceCompletion struct {
	paths   []string
	err     error
	limited bool
}

func indexWorkspace(dir string) *workspaceCompletion {
	index := &workspaceCompletion{}
	excluded := map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "dist": true, "build": true, "target": true, ".next": true, "__pycache__": true}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		index.err = err
		return index
	}
	index.err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == canonical {
			return nil
		}
		if excluded[entry.Name()] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(canonical, target)
			if err != nil || !filepath.IsLocal(rel) {
				return nil
			}
			// Do not follow or suggest symlink directories.
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				return nil
			}
		}
		name := filepath.ToSlash(relative)
		if strings.ContainsAny(name, "\r\n\x1b") {
			return nil
		}
		if entry.IsDir() {
			name += "/"
		}
		index.paths = append(index.paths, name)
		if len(index.paths) >= 50000 {
			index.limited = true
			return fs.SkipAll
		}
		return nil
	})
	return index
}

// completionRank orders a completion candidate for fragment: 0 for an
// exact match, 1 for a prefix, 2 for a subsequence, and 3 for no match.
// Case does not matter.
func completionRank(fragment, name string) int {
	fragment, name = strings.ToLower(fragment), strings.ToLower(name)
	if name == fragment {
		return 0
	}
	if strings.HasPrefix(name, fragment) {
		return 1
	}
	remaining := []rune(fragment)
	for _, r := range name {
		if len(remaining) > 0 && r == remaining[0] {
			remaining = remaining[1:]
		}
	}
	if len(remaining) == 0 {
		return 2
	}
	return 3
}

func rankCompletion(fragment string, names []string) []string {
	rank := func(name string) int { return completionRank(fragment, name) }
	var out []string
	for _, name := range names {
		if rank(name) < 3 {
			out = append(out, name)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if delta := rank(a) - rank(b); delta != 0 {
			return delta
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(out)
}
func (s *screen) completeDraft(value string) []string {
	s.exitGuard.reset()
	// Tab in the open command menu completes the selected command.
	if name := s.commandMenu.complete; name != "" {
		s.commandMenu.complete = ""
		return []string{name + " "}
	}
	leading := len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace))
	rest := value[leading:]
	var candidates []string
	completeIndex := true
	marker := trailingPathMarker(value)
	commandEnd := strings.IndexFunc(rest, unicode.IsSpace)
	pathInArguments := commandEnd >= 0 && marker >= leading+commandEnd
	if args, ok := strings.CutPrefix(rest, "/pane "); ok && s.Panes() {
		candidates = s.completePane(value[:leading]+"/pane ", args)
	} else if strings.HasPrefix(rest, "/") && !pathInArguments {
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			end = len(rest)
		}
		for _, name := range rankCompletion(rest[:end], s.ctrl.commandNames()) {
			candidates = append(candidates, value[:leading]+name+rest[end:])
		}
	} else if marker >= 0 {
		fragment := value[marker+1:]
		if fragment != "" && (!filepath.IsLocal(filepath.FromSlash(fragment)) || strings.Contains(fragment, "\\") || strings.Contains(fragment, ".."+string(filepath.Separator))) {
			s.Emit(warnBlock("Completion paths must stay inside the workspace."))
			return nil
		}
		if s.pathCompletion == nil {
			s.pathCompletion = indexWorkspace(s.dir)
		}
		completeIndex = s.pathCompletion.err == nil && !s.pathCompletion.limited
		if s.pathCompletion.err != nil {
			s.Emit(warnBlock("Workspace completion is incomplete: %v", s.pathCompletion.err))
		}
		if s.pathCompletion.limited {
			s.Emit(warnBlock("Workspace completion indexed only the first 50,000 paths."))
		}
		for _, name := range rankCompletion(fragment, s.pathCompletion.paths) {
			candidates = append(candidates, value[:marker]+"@"+name)
		}
	}
	if len(candidates) == 0 && completeIndex {
		s.Emit(warnBlock("No completion exists for this draft."))
	}
	if len(candidates) > 20 {
		s.Emit(warnBlock("Completion shows the first %d of %d matches.", 20, len(candidates)))
		candidates = candidates[:20]
	}
	return candidates
}
func trailingPathMarker(text string) int {
	marker := -1
	previousSpace := true
	for offset, r := range text {
		if r == '@' && previousSpace {
			marker = offset
		}
		previousSpace = unicode.IsSpace(r)
	}
	return marker
}

// paneActions are the actions /pane takes, in the order help lists them.
var paneActions = []string{"close", "zoom", "equalize", "next", "focus"}

// completePane completes the argument of /pane: an action, or after focus
// the name of an open pane. prefix is the draft up to the argument.
func (s *screen) completePane(prefix, args string) []string {
	var out []string
	args = strings.TrimLeftFunc(args, unicode.IsSpace)
	if name, ok := strings.CutPrefix(args, "focus "); ok {
		var names []string
		for _, id := range s.panes.leaves() {
			names = append(names, s.panes.get(id).label)
		}
		for _, n := range rankCompletion(strings.TrimSpace(name), names) {
			out = append(out, prefix+"focus "+n)
		}
		return out
	}
	if strings.ContainsFunc(args, unicode.IsSpace) {
		return nil
	}
	for _, a := range rankCompletion(args, paneActions) {
		if a == "focus" {
			a += " "
		}
		out = append(out, prefix+a)
	}
	return out
}
