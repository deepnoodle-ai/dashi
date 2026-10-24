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
	}
	if err := checkGlobs(in.Pattern, patterns); err != nil {
		return fail(err.Error())
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

// checkGlobs returns an error, written for the model, when one of patterns,
// expanded from typed, has bad syntax such as an unclosed [.
func checkGlobs(typed string, patterns []string) error {
	for _, p := range patterns {
		if _, err := path.Match(strings.ReplaceAll(p, "**", "*"), ""); err != nil {
			return fmt.Errorf("pattern %q is not valid: %v", typed, err)
		}
	}
	return nil
}

// matchGlob matches a slash-separated name against a pattern in which a **
// segment matches zero or more directories.
func matchGlob(pattern, name string) bool {
	// Repeated ** segments match what one does, so collapse them.
	var pat []string
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" && len(pat) > 0 && pat[len(pat)-1] == "**" {
			continue
		}
		pat = append(pat, seg)
	}
	return matchSegments(pat, strings.Split(name, "/"))
}

// matchSegments reports whether pat matches name, segment by segment. It
// remembers each (pattern position, name position) pair that failed, so a
// pattern with many ** segments, such as **/a/**/a/**/x, takes at most
// len(pat)*len(name) steps instead of backtracking exponentially.
func matchSegments(pat, name []string) bool {
	width := len(name) + 1
	failed := make([]bool, (len(pat)+1)*width)
	var match func(i, j int) bool
	match = func(i, j int) bool {
		if failed[i*width+j] {
			return false
		}
		ok := false
		switch {
		case i == len(pat):
			ok = j == len(name)
		case pat[i] == "**":
			for k := j; k <= len(name) && !ok; k++ {
				ok = match(i+1, k)
			}
		case j < len(name):
			if m, _ := path.Match(pat[i], name[j]); m {
				ok = match(i+1, j+1)
			}
		}
		if !ok {
			failed[i*width+j] = true
		}
		return ok
	}
	return match(0, 0)
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
