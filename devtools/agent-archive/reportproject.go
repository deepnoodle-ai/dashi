package main

import (
	"path"
	"strings"
)

// Worktree folders, which the report rolls up into their project
// (decision 6).
const (
	claudeWorktrees = "/.claude/worktrees/"
	codexWorktrees  = "/.codex/worktrees/"
)

// projectDir is a working_dir and the git remotes of its sessions.
type projectDir struct {
	Dir     string
	Remotes map[string]bool
}

// projects maps each working_dir to its project, home printed as ~.
//
//   - …/.claude/worktrees/<name>[/…] becomes the part before
//     /.claude/worktrees.
//   - …/.codex/worktrees/<id>/<name>[/…] becomes the one other project
//     whose last element is <name>; when several match, the one whose
//     sessions share the worktree session's git remote; when none or
//     several still match, …/.codex/worktrees/*/<name>.
//
// A project's subfolders are not merged.
func projects(dirs []projectDir, home string) map[string]string {
	out := map[string]string{}
	byName := map[string][]string{}         // last element to projects
	remotes := map[string]map[string]bool{} // project to remotes
	for _, d := range dirs {
		if isCodexWorktree(d.Dir) {
			continue
		}
		p := d.Dir
		if i := strings.Index(p, claudeWorktrees); i >= 0 {
			p = p[:i]
		}
		out[d.Dir] = p
		if remotes[p] == nil {
			remotes[p] = map[string]bool{}
			if p != "" {
				byName[path.Base(p)] = append(byName[path.Base(p)], p)
			}
		}
		for r := range d.Remotes {
			remotes[p][r] = true
		}
	}
	for _, d := range dirs {
		if !isCodexWorktree(d.Dir) {
			continue
		}
		i := strings.Index(d.Dir, codexWorktrees)
		rest := strings.Split(d.Dir[i+len(codexWorktrees):], "/")
		if len(rest) < 2 || rest[1] == "" {
			out[d.Dir] = d.Dir
			continue
		}
		name := rest[1]
		match := byName[name]
		if len(match) > 1 {
			var shared []string
			for _, m := range match {
				for r := range d.Remotes {
					if r != "" && remotes[m][r] {
						shared = append(shared, m)
						break
					}
				}
			}
			match = shared
		}
		if len(match) == 1 {
			out[d.Dir] = match[0]
		} else {
			out[d.Dir] = d.Dir[:i] + codexWorktrees + "*/" + name
		}
	}
	for dir, p := range out {
		out[dir] = tildePath(p, home)
	}
	return out
}

func isCodexWorktree(dir string) bool { return strings.Contains(dir, codexWorktrees) }

// tildePath prints home as ~.
func tildePath(p, home string) string {
	switch {
	case p == "":
		return "(none)"
	case home == "":
		return p
	case p == home:
		return "~"
	case strings.HasPrefix(p, home+"/"):
		return "~" + p[len(home):]
	}
	return p
}
