package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

const defaultGrepLimit = 250

type grepArgs struct {
	Pattern    string `json:"pattern" description:"Regular expression to search for."`
	Path       string `json:"path,omitempty" description:"File or directory to search. Default is the workspace root."`
	Glob       string `json:"glob,omitempty" description:"Only search files whose path matches this glob, such as *.go."`
	Mode       string `json:"output_mode,omitempty" description:"content shows matching lines. files shows file paths. count shows the number of matches per file. Default is files." enum:"content,files,count"`
	IgnoreCase bool   `json:"ignore_case,omitempty" description:"Match without regard to case."`
	Context    int    `json:"context,omitempty" description:"Lines of context around each match. Only for content mode."`
	Limit      int    `json:"head_limit,omitempty" description:"Maximum number of result lines. Default 250."`
}

func newGrep(s *scope) (tool.Tool, error) {
	rg, _ := exec.LookPath("rg")
	t, err := tool.Func(
		ToolGrep,
		"Search file contents with a regular expression. Skip hidden files "+
			"and files that .gitignore lists. Result paths are relative to the "+
			"workspace. Use glob to limit the files searched.",
		func(ctx context.Context, _ tool.Call, in grepArgs) (tool.Outcome, error) {
			return grep(ctx, s, rg, in)
		},
	)
	return annotate(t, true), err
}

func grep(
	ctx context.Context,
	s *scope,
	rg string,
	in grepArgs,
) (tool.Outcome, error) {
	if in.Pattern == "" {
		return fail("pattern is empty. Give a regular expression.")
	}
	mode := in.Mode
	if mode == "" {
		mode = "files"
	}
	if in.Context < 0 || in.Limit < 0 {
		return fail("context and head_limit must not be negative.")
	}
	limit := in.Limit
	if limit == 0 {
		limit = defaultGrepLimit
	}
	target, err := s.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	if _, err := os.Stat(target); err != nil {
		return fail(fmt.Sprintf("path %s does not exist.", in.Path))
	}
	re, err := regexp.Compile(regexpFlags(in) + in.Pattern)
	if err != nil {
		return fail(fmt.Sprintf("pattern is not a valid regular expression: %v", err))
	}
	var lines []string
	if rg != "" {
		lines, err = ripgrep(ctx, s, rg, target, in, mode)
	} else {
		lines, err = goSearch(s, re, target, in, mode)
	}
	if err != nil {
		return fail(err.Error())
	}
	if len(lines) == 0 {
		return tool.Succeeded("No matches."), nil
	}
	note := ""
	if len(lines) > limit {
		note = fmt.Sprintf("\n[Showed %d of %d lines. Narrow the search or raise head_limit.]", limit, len(lines))
		lines = lines[:limit]
	}
	return tool.Succeeded(strings.Join(lines, "\n") + note), nil
}

func regexpFlags(in grepArgs) string {
	if in.IgnoreCase {
		return "(?i)"
	}
	return ""
}

// ripgrep runs rg from the workspace root so that paths come out relative to
// it. The Go search produces the same line formats: path:line:text for
// content, path-line-text for context, path for files, and path:count.
func ripgrep(
	ctx context.Context,
	s *scope,
	rg, target string,
	in grepArgs,
	mode string,
) ([]string, error) {
	args := []string{"--no-heading", "--color", "never", "--sort", "path"}
	switch mode {
	case "content":
		args = append(args, "--line-number", "--with-filename")
		if in.Context > 0 {
			args = append(args, "-C", fmt.Sprint(in.Context))
		}
	case "files":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count", "--with-filename")
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "-e", in.Pattern, "--")
	if target != s.root {
		args = append(args, s.rel(target))
	}
	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Dir = s.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("search failed: %s", strings.TrimSpace(stderr.String()))
	}
	return strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n"), nil
}

func goSearch(
	s *scope,
	re *regexp.Regexp,
	target string,
	in grepArgs,
	mode string,
) ([]string, error) {
	var lines []string
	search := func(p string) {
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return
		}
		rel := s.rel(p)
		if in.Glob != "" && !globMatchesPath(in.Glob, rel) {
			return
		}
		lines = append(lines, searchFile(re, rel, string(data), in.Context, mode)...)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		search(target)
		return lines, nil
	}
	err = filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && p != target &&
			(strings.HasPrefix(d.Name(), ".") || slices.Contains(skipDirs, d.Name())):
			return filepath.SkipDir
		case d.Type().IsRegular() && !strings.HasPrefix(d.Name(), "."):
			search(p)
		}
		return nil
	})
	return lines, err
}

func globMatchesPath(pattern, rel string) bool {
	for _, p := range expandBraces(pattern) {
		if !strings.Contains(p, "/") {
			p = "**/" + p
		}
		if matchGlob(p, rel) {
			return true
		}
	}
	return false
}

func searchFile(
	re *regexp.Regexp,
	rel, text string,
	context int,
	mode string,
) []string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(nil, 1<<24)
	var all []string
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	var hits []int
	for i, line := range all {
		if re.MatchString(line) {
			hits = append(hits, i)
		}
	}
	switch {
	case len(hits) == 0:
		return nil
	case mode == "files":
		return []string{rel}
	case mode == "count":
		return []string{fmt.Sprintf("%s:%d", rel, len(hits))}
	}
	var out []string
	last := -1
	for _, h := range hits {
		from, to := max(h-context, 0), min(h+context, len(all)-1)
		if last >= 0 && from > last+1 && context > 0 {
			out = append(out, "--")
		}
		for i := max(from, last+1); i <= to; i++ {
			sep := "-"
			if slices.Contains(hits, i) {
				sep = ":"
			}
			out = append(out, fmt.Sprintf("%s%s%d%s%s", rel, sep, i+1, sep, all[i]))
		}
		last = max(last, to)
	}
	return out
}
