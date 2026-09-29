package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestToolsListsExpectedNames(t *testing.T) {
	names := func(list tool.List) []string {
		var out []string
		for _, tl := range list {
			out = append(out, tl.Definition.Name)
		}
		return out
	}
	list, _ := newTestTools(t)
	if got := strings.Join(names(list), ","); got !=
		"read_file,list_directory,glob,grep,write_file,edit_file,bash" {
		t.Fatalf("names = %s", got)
	}
	ask := func(context.Context, Question) (Answer, error) { return Answer{}, nil }
	list, _ = newTestTools(t, WithAsker(ask), WithoutTools(ToolBash, "nope"))
	if got := strings.Join(names(list), ","); got !=
		"read_file,list_directory,glob,grep,write_file,edit_file,ask_user" {
		t.Fatalf("names = %s", got)
	}
}

func TestToolsMarkReadOnly(t *testing.T) {
	ask := func(context.Context, Question) (Answer, error) { return Answer{}, nil }
	list, _ := newTestTools(t, WithAsker(ask))
	mutating := map[string]bool{ToolWriteFile: true, ToolEditFile: true, ToolBash: true}
	for _, tl := range list {
		ro := tl.Definition.Annotations.ReadOnly
		if ro == nil || *ro == mutating[tl.Definition.Name] {
			t.Errorf("%s: ReadOnly = %v", tl.Definition.Name, ro)
		}
	}
}

func TestReadFileNumbersLinesAndPages(t *testing.T) {
	list, dir := newTestTools(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	writeTestFile(t, dir, "a.txt", strings.Join(lines, "\n")+"\n")
	out, text := call(t, list, ToolReadFile, map[string]any{"path": "a.txt", "offset": 3, "limit": 2})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "     3\tline 3\n     4\tline 4\n")
	wantContains(t, text, "Set offset to 5")
	if strings.Contains(text, "line 5") {
		t.Fatalf("read past the limit: %s", text)
	}
	out, text = call(t, list, ToolReadFile, map[string]any{"path": "a.txt", "offset": 99})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "past the end")
}

func TestReadFileErrorsTellTheNextStep(t *testing.T) {
	list, dir := newTestTools(t)
	writeTestFile(t, dir, "bin", "a\x00b")
	writeTestFile(t, dir, "empty", "")
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, want string
		state            tool.State
	}{
		{"missing", "nope.txt", "does not exist", tool.StateFailed},
		{"binary", "bin", "binary file", tool.StateFailed},
		{"directory", "d", "list_directory", tool.StateFailed},
		{"empty", "empty", "empty", tool.StateSucceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, text := call(t, list, ToolReadFile, map[string]any{"path": tc.path})
			wantState(t, out, text, tc.state)
			wantContains(t, text, tc.want)
		})
	}
}

func TestListDirectoryMarksDirectories(t *testing.T) {
	list, dir := newTestTools(t)
	writeTestFile(t, dir, "b.txt", "")
	writeTestFile(t, dir, "a/x.txt", "")
	_, text := call(t, list, ToolListDirectory, map[string]any{})
	if text != "a/\nb.txt" {
		t.Fatalf("text = %q", text)
	}
}

func TestGlobMatchesRecursivelyNewestFirst(t *testing.T) {
	list, dir := newTestTools(t)
	old := writeTestFile(t, dir, "old.go", "")
	writeTestFile(t, dir, "pkg/deep/new.go", "")
	writeTestFile(t, dir, "pkg/readme.md", "")
	writeTestFile(t, dir, "node_modules/x/skip.go", "")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	_, text := call(t, list, ToolGlob, map[string]any{"pattern": "**/*.go"})
	if text != "pkg/deep/new.go\nold.go" {
		t.Fatalf("text = %q", text)
	}
	_, text = call(t, list, ToolGlob, map[string]any{"pattern": "*.{md,go}", "path": "pkg"})
	wantContains(t, text, "pkg/readme.md")
	wantContains(t, text, "pkg/deep/new.go")
	_, text = call(t, list, ToolGlob, map[string]any{"pattern": "*.zig"})
	wantContains(t, text, "No files match")
}

func TestWriteFileCreatesParents(t *testing.T) {
	list, dir := newTestTools(t)
	out, text := call(t, list, ToolWriteFile, map[string]any{"path": "a/b/c.txt", "content": "hi"})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "Created")
	data, err := os.ReadFile(filepath.Join(dir, "a/b/c.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
	_, text = call(t, list, ToolWriteFile, map[string]any{"path": "a/b/c.txt", "content": "yo"})
	wantContains(t, text, "Overwrote")
	out, text = call(t, list, ToolWriteFile, map[string]any{"path": "a", "content": "x"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "directory")
}

func TestEditFileReplacesUniqueString(t *testing.T) {
	list, dir := newTestTools(t)
	path := writeTestFile(t, dir, "m.go", "a\nb\nc\n")
	out, text := call(t, list, ToolEditFile, map[string]any{"path": "m.go", "old_string": "b", "new_string": "B"})
	wantState(t, out, text, tool.StateSucceeded)
	if data, _ := os.ReadFile(path); string(data) != "a\nB\nc\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestEditFileErrorsTellTheModelHowToFix(t *testing.T) {
	list, dir := newTestTools(t)
	path := writeTestFile(t, dir, "m.go", "x\ny\nx\nz\nx\n")
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"repeated", map[string]any{"path": "m.go", "old_string": "x", "new_string": "q"},
			"appears 3 times (lines 1, 3, 5). Add surrounding lines"},
		{"missing", map[string]any{"path": "m.go", "old_string": "nope", "new_string": "q"},
			"old_string not found in m.go"},
		{"same", map[string]any{"path": "m.go", "old_string": "x", "new_string": "x"},
			"the same"},
		{"empty old", map[string]any{"path": "m.go", "old_string": "", "new_string": "q"},
			"old_string is empty"},
		{"no file", map[string]any{"path": "no.go", "old_string": "x", "new_string": "q"},
			"write_file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, text := call(t, list, ToolEditFile, tc.args)
			wantState(t, out, text, tool.StateFailed)
			wantContains(t, text, tc.want)
		})
	}
	if data, _ := os.ReadFile(path); string(data) != "x\ny\nx\nz\nx\n" {
		t.Fatalf("a failed edit changed the file: %q", data)
	}
	out, text := call(t, list, ToolEditFile, map[string]any{"path": "m.go", "old_string": "x", "new_string": "q", "replace_all": true})
	wantState(t, out, text, tool.StateSucceeded)
	if data, _ := os.ReadFile(path); string(data) != "q\ny\nq\nz\nq\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestGrepFindsMatchesWithBothEngines(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.go", "package a\n// TODO one\nfunc A() {}\n")
	writeTestFile(t, dir, "sub/b.go", "package b\n// todo two\n// TODO three\n")
	writeTestFile(t, dir, "sub/c.txt", "TODO in text\n")
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	engines := map[string]string{"go": ""}
	if rg, err := exec.LookPath("rg"); err == nil {
		engines["rg"] = rg
	}
	tests := []struct {
		name string
		args grepArgs
		want string
	}{
		{"files", grepArgs{Pattern: "TODO"}, "a.go\nsub/b.go\nsub/c.txt"},
		{"count", grepArgs{Pattern: "TODO", Mode: "count", Glob: "*.go"}, "a.go:1\nsub/b.go:1"},
		{"ignore case", grepArgs{Pattern: "todo", Mode: "count", IgnoreCase: true, Path: "sub"}, "sub/b.go:2\nsub/c.txt:1"},
		{"content", grepArgs{Pattern: "TODO", Mode: "content", Glob: "*.go"}, "a.go:2:// TODO one\nsub/b.go:3:// TODO three"},
		{"context", grepArgs{Pattern: "func", Mode: "content", Context: 1}, "a.go-2-// TODO one\na.go:3:func A() {}"},
		{"none", grepArgs{Pattern: "zzz"}, "No matches."},
	}
	for engine, rg := range engines {
		for _, tc := range tests {
			t.Run(engine+" "+tc.name, func(t *testing.T) {
				out, err := grep(context.Background(), s, rg, tc.args)
				if err != nil {
					t.Fatal(err)
				}
				text := out.Output.Content[0].Text.Text
				if text != tc.want {
					t.Fatalf("text = %q, want %q", text, tc.want)
				}
			})
		}
	}
}

func TestGrepReportsBadPattern(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolGrep, map[string]any{"pattern": "("})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "regular expression")
}

func TestGrepLimitsResults(t *testing.T) {
	list, dir := newTestTools(t)
	writeTestFile(t, dir, "a.txt", strings.Repeat("hit\n", 10))
	_, text := call(t, list, ToolGrep, map[string]any{"pattern": "hit", "output_mode": "content", "head_limit": 3})
	wantContains(t, text, "Showed the first 3 lines")
}

func TestGrepBoundsRetainedOutput(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", strings.Repeat(strings.Repeat("x", 5000)+"\n", 1000))
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	engines := map[string]string{"go": ""}
	if rg, err := exec.LookPath("rg"); err == nil {
		engines["rg"] = rg
	}
	for engine, rg := range engines {
		t.Run(engine, func(t *testing.T) {
			out, err := grep(context.Background(), s, rg, grepArgs{Pattern: "x", Mode: "content", Limit: 100000})
			if err != nil {
				t.Fatal(err)
			}
			text := out.Output.Content[0].Text.Text
			if len(text) > grepMaxBytes+1000 {
				t.Fatalf("result holds %d bytes, want at most %d", len(text), grepMaxBytes)
			}
			wantContains(t, text, "More results exist")
		})
	}
}

func TestLineSinkCapsLineLength(t *testing.T) {
	k := &lineSink{limit: 5}
	k.add(strings.Repeat("y", 10000))
	if len(k.lines[0]) > grepMaxLine+3 {
		t.Fatalf("line holds %d bytes", len(k.lines[0]))
	}
}
