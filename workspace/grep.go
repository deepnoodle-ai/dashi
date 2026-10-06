package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	defaultGrepLimit = 250
	// grepMaxBytes bounds the result text kept in memory. grepMaxLine
	// bounds one result line.
	grepMaxBytes = 256 << 10
	grepMaxLine  = 2000
)

// lineSink keeps at most limit lines and grepMaxBytes of text. It records
// that more output existed. Output past the bound is counted, not kept.
type lineSink struct {
	limit int
	lines []string
	size  int
	full  bool
}

// add reports whether the caller should keep producing lines.
func (k *lineSink) add(l string) bool {
	if k.full {
		return false
	}
	if len(l) > grepMaxLine {
		l = strings.ToValidUTF8(l[:grepMaxLine], "") + "..."
	}
	if len(k.lines) >= k.limit || k.size+len(l) > grepMaxBytes {
		k.full = true
		return false
	}
	k.lines = append(k.lines, l)
	k.size += len(l)
	return true
}

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
	return closedWorld(tool.Func(
		ToolGrep,
		"Search file contents with a regular expression. Skip hidden files "+
			"and common dependency and build directories. Result paths are "+
			"relative to the workspace. Use glob to limit the files searched.",
		func(ctx context.Context, _ tool.Call, in grepArgs) (tool.Outcome, error) {
			return grep(ctx, s, rg, in)
		},
		tool.ReadOnly(),
	))
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
	sink := &lineSink{limit: limit}
	if rg != "" {
		err = ripgrep(ctx, s, rg, target, in, mode, sink)
	} else {
		err = goSearch(s, re, target, in, mode, sink)
	}
	if err != nil {
		return fail(err.Error())
	}
	if len(sink.lines) == 0 {
		return tool.Succeeded("No matches."), nil
	}
	note := ""
	if sink.full {
		note = fmt.Sprintf("\n[Showed the first %d lines. More results exist. Narrow the search or raise head_limit.]", len(sink.lines))
	}
	return tool.Succeeded(strings.Join(sink.lines, "\n") + note), nil
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
	sink *lineSink,
) error {
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Dir = s.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("search failed: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("search failed: %v", err)
	}
	r := bufio.NewReaderSize(pipe, 64<<10)
	for {
		// Keep the first chunk of a long line and drop the rest.
		chunk, isPrefix, rerr := r.ReadLine()
		if rerr != nil {
			break
		}
		line := string(chunk)
		for isPrefix {
			_, isPrefix, _ = r.ReadLine()
		}
		if !sink.add(line) {
			cancel() // enough output; stop the search
			break
		}
	}
	_, _ = io.Copy(io.Discard, pipe)
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case sink.full:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return nil
	case err != nil:
		return fmt.Errorf("search failed: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func goSearch(
	s *scope,
	re *regexp.Regexp,
	target string,
	in grepArgs,
	mode string,
	sink *lineSink,
) error {
	search := func(p string) {
		if sink.full {
			return
		}
		rel := s.rel(p)
		if in.Glob != "" && !globMatchesPath(in.Glob, rel) {
			return
		}
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		head := make([]byte, 8000)
		k, _ := io.ReadFull(f, head)
		if bytes.IndexByte(head[:k], 0) >= 0 {
			return
		}
		r := io.MultiReader(bytes.NewReader(head[:k]), f)
		searchFile(r, re, rel, in.Context, mode, sink.add)
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		search(target)
		return nil
	}
	err = filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		switch {
		case sink.full:
			return filepath.SkipAll
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
	return err
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

// grepMatchLine is the most bytes of one line that the search keeps in
// memory. Longer lines are cut before matching, as with ripgrep output.
const grepMatchLine = 64 << 10

type numberedLine struct {
	n    int
	text string
}

// searchFile reads r one line at a time and passes each result line to emit.
// It stops when emit returns false. It keeps only the lines of pending
// context and a match count, so memory use does not depend on file size.
func searchFile(
	r io.Reader,
	re *regexp.Regexp,
	rel string,
	context int,
	mode string,
	emit func(string) bool,
) {
	br := bufio.NewReaderSize(r, 64<<10)
	var before []numberedLine
	after, last, count := 0, 0, 0
	for n := 1; ; n++ {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			break
		}
		text := string(chunk)
		for isPrefix {
			_, isPrefix, _ = br.ReadLine()
		}
		if len(text) > grepMatchLine {
			text = text[:grepMatchLine]
		}
		if re.MatchString(text) {
			switch mode {
			case "files":
				emit(rel)
				return
			case "count":
				count++
				continue
			}
			first := n
			if len(before) > 0 {
				first = before[0].n
			}
			if last > 0 && context > 0 && first > last+1 && !emit("--") {
				return
			}
			for _, b := range before {
				if !emit(fmt.Sprintf("%s-%d-%s", rel, b.n, b.text)) {
					return
				}
			}
			before = before[:0]
			if !emit(fmt.Sprintf("%s:%d:%s", rel, n, text)) {
				return
			}
			last, after = n, context
			continue
		}
		if mode != "content" || context == 0 {
			continue
		}
		if after > 0 {
			if !emit(fmt.Sprintf("%s-%d-%s", rel, n, text)) {
				return
			}
			last = n
			after--
			continue
		}
		if len(before) == context {
			before = before[1:]
		}
		before = append(before, numberedLine{n, text})
	}
	if mode == "count" && count > 0 {
		emit(fmt.Sprintf("%s:%d", rel, count))
	}
}
