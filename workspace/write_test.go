package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestEditFileRefusesHugeFile(t *testing.T) {
	list, dir := newTestTools(t)
	f, err := os.Create(filepath.Join(dir, "huge.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file: large, but it takes no disk space.
	if err := f.Truncate(editMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, text := call(t, list, ToolEditFile, map[string]any{"path": "huge.txt", "old_string": "a", "new_string": "b"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "more than the 33554432 bytes")
}
