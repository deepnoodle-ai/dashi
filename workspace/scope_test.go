package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestScopeRefusesPathsOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, outside, "secret.txt", "secret")
	links := map[string]string{
		"link-file":   filepath.Join(outside, "secret.txt"),
		"link-dir":    outside,
		"link-new":    filepath.Join(outside, "new.txt"),
		"link-parent": "..",
		"link-chain":  "link-dir",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"parent", "../outside/secret.txt"},
		{"parent of root", ".."},
		{"absolute", filepath.Join(outside, "secret.txt")},
		{"symlink to file", "link-file"},
		{"symlink to dir", "link-dir/secret.txt"},
		{"symlink to new file", "link-new"},
		{"symlinked parent of new file", "link-dir/created.txt"},
		{"link to parent", "link-parent/outside/secret.txt"},
		{"chained link", "link-chain/secret.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.resolve(tc.path)
			if err == nil {
				t.Fatalf("resolve(%q) succeeded", tc.path)
			}
			if !strings.Contains(err.Error(), "outside the workspace") {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

func TestScopeAllowsPathsInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a/b.txt", "x")
	if err := os.Symlink("a", filepath.Join(dir, "inner")); err != nil {
		t.Fatal(err)
	}
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", ".", "a/b.txt", "a/new/deep.txt", "inner/b.txt", "a/../a/b.txt", filepath.Join(dir, "a", "b.txt")} {
		if _, err := s.resolve(p); err != nil {
			t.Errorf("resolve(%q): %v", p, err)
		}
	}
}

func TestFileToolsRefuseEscape(t *testing.T) {
	list, dir := newTestTools(t)
	writeTestFile(t, filepath.Dir(dir), "secret.txt", "secret")
	tests := []struct {
		tool string
		args map[string]any
	}{
		{ToolReadFile, map[string]any{"path": "../secret.txt"}},
		{ToolListDirectory, map[string]any{"path": ".."}},
		{ToolGlob, map[string]any{"pattern": "*", "path": ".."}},
		{ToolGrep, map[string]any{"pattern": "secret", "path": ".."}},
		{ToolWriteFile, map[string]any{"path": "../new.txt", "content": "x"}},
		{ToolEditFile, map[string]any{"path": "../secret.txt", "old_string": "secret", "new_string": "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			out, text := call(t, list, tc.tool, tc.args)
			wantState(t, out, text, tool.StateFailed)
			wantContains(t, text, "outside the workspace")
		})
	}
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(dir), "secret.txt"))
	if string(data) != "secret" {
		t.Fatal("a tool changed a file outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "new.txt")); err == nil {
		t.Fatal("write_file created a file outside the workspace")
	}
}

func TestNewRefusesMissingDirectory(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("New accepted a missing directory")
	}
	file := writeTestFile(t, t.TempDir(), "f", "x")
	if _, err := New(file); err == nil {
		t.Fatal("New accepted a file")
	}
}
