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

// maxBracePatterns caps the patterns that brace expansion may produce. Each
// group multiplies the count, so ten {a,b} groups already make 1024.
const maxBracePatterns = 64

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
		func(ctx context.Context, _ tool.Call, in globArgs) (tool.Outcome, error) {
			return glob(ctx, s, in)
		},
		tool.ReadOnly(),
	))
}

func glob(ctx context.Context, s *scope, in globArgs) (tool.Outcome, error) {
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
	patterns, err := expandBraces(in.Pattern)
	if err != nil {
		return fail(err.Error())
	}
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
		if ctx.Err() != nil {
			return fail("The search was canceled.")
		}
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

// expandBraces expands each {a,b} group of p, including nested groups.
// Models often write *.{ts,tsx}, which path.Match does not support. A { with
// no matching } stays literal. The error, written for the model, reports a
// pattern that would expand to more than maxBracePatterns patterns.
func expandBraces(p string) ([]string, error) {
	var out []string
	if !expandBracesInto(p, &out) {
		return nil, fmt.Errorf(
			"pattern %q expands to more than %d patterns. Use fewer {a,b} groups.",
			p, maxBracePatterns,
		)
	}
	return out, nil
}

// expandBracesInto appends the expansions of p to out. It reports false, and
// stops early, when out would grow past maxBracePatterns.
func expandBracesInto(p string, out *[]string) bool {
	open, end, alts := firstBraceGroup(p)
	if open < 0 {
		if len(*out) >= maxBracePatterns {
			return false
		}
		*out = append(*out, p)
		return true
	}
	for _, alt := range alts {
		if !expandBracesInto(p[:open]+alt+p[end+1:], out) {
			return false
		}
	}
	return true
}

// firstBraceGroup finds the first { that has a matching }, and splits the
// text between them at its top-level commas. open is -1 when p has no group.
func firstBraceGroup(p string) (open, end int, alts []string) {
	for open = strings.IndexByte(p, '{'); open >= 0; {
		depth, start := 0, open+1
		for i := open; i < len(p); i++ {
			switch p[i] {
			case '{':
				depth++
			case ',':
				if depth == 1 {
					alts = append(alts, p[start:i])
					start = i + 1
				}
			case '}':
				depth--
				if depth == 0 {
					return open, i, append(alts, p[start:i])
				}
			}
		}
		// This { has no match. Try a later one.
		alts = nil
		next := strings.IndexByte(p[open+1:], '{')
		if next < 0 {
			break
		}
		open += 1 + next
	}
	return -1, -1, nil
}
