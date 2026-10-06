package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
)

const maxGlobResults = 1000

// skipDirs are directories that glob and the Go search never enter. They hold
// dependencies and version-control data, which bury the results the model
// wants.
var skipDirs = []string{".git", "node_modules", "vendor", "__pycache__", ".venv"}

type globArgs struct {
	Pattern string `json:"pattern" description:"Glob pattern, such as **/*.go or src/*.{ts,tsx}. A pattern with no / matches names at any depth."`
	Path    string `json:"path,omitempty" description:"Directory to search. Default is the workspace root."`
}

func newGlob(s *scope) (tool.Tool, error) {
	return closedWorld(tool.Func(
		ToolGlob,
		"Find files by name pattern. ** matches any number of directories. "+
			"The result lists paths relative to the workspace, newest first.",
		func(_ context.Context, _ tool.Call, in globArgs) (tool.Outcome, error) {
			return glob(s, in)
		},
		tool.ReadOnly(),
	))
}

func glob(s *scope, in globArgs) (tool.Outcome, error) {
	if in.Pattern == "" {
		return fail("pattern is empty. Give a pattern such as **/*.go.")
	}
	dir, err := s.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fail(fmt.Sprintf("%s is not a directory that exists.", in.Path))
	}
	patterns := expandBraces(in.Pattern)
	for i, p := range patterns {
		if !strings.Contains(p, "/") {
			patterns[i] = "**/" + p
		}
		if _, err := path.Match(strings.ReplaceAll(patterns[i], "**", "*"), ""); err != nil {
			return fail(fmt.Sprintf("pattern %q is not valid: %v", in.Pattern, err))
		}
	}
	type match struct {
		name string
		mod  time.Time
	}
	var matches []match
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && slices.Contains(skipDirs, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		for _, pat := range patterns {
			if matchGlob(pat, rel) {
				info, _ := d.Info()
				var mod time.Time
				if info != nil {
					mod = info.ModTime()
				}
				matches = append(matches, match{s.rel(p), mod})
				break
			}
		}
		return nil
	}
	if err := filepath.WalkDir(dir, walk); err != nil {
		return fail(fmt.Sprintf("cannot search %s: %v", in.Path, err))
	}
	if len(matches) == 0 {
		return tool.Succeeded("No files match."), nil
	}
	slices.SortStableFunc(matches, func(a, b match) int {
		return b.mod.Compare(a.mod)
	})
	var names []string
	for _, m := range matches {
		names = append(names, m.name)
	}
	note := ""
	if len(names) > maxGlobResults {
		note = fmt.Sprintf("\n[Showed %d of %d files. Use a narrower pattern or path.]", maxGlobResults, len(names))
		names = names[:maxGlobResults]
	}
	return tool.Succeeded(strings.Join(names, "\n") + note), nil
}

// matchGlob matches a slash-separated name against a pattern in which a **
// segment matches zero or more directories.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// expandBraces expands the first {a,b} group of p, recursively. Models often
// write *.{ts,tsx}, which path.Match does not support.
func expandBraces(p string) []string {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		return []string{p}
	}
	end := strings.IndexByte(p[open:], '}')
	if end < 0 {
		return []string{p}
	}
	end += open
	var out []string
	for _, alt := range strings.Split(p[open+1:end], ",") {
		out = append(out, expandBraces(p[:open]+alt+p[end+1:])...)
	}
	return out
}
