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

// read_file shows CRLF lines without their \r, so a multi-line old_string
// the model copies from it uses \n. The edit still matches and keeps CRLF.
func TestEditFileMatchesCRLFLines(t *testing.T) {
	list, dir := newTestTools(t)
	path := writeTestFile(t, dir, "crlf.txt", "first\r\nsecond\r\nthird\r\n")
	out, text := call(t, list, ToolEditFile, map[string]any{
		"path": "crlf.txt", "old_string": "first\nsecond", "new_string": "first\nchanged",
	})
	wantState(t, out, text, tool.StateSucceeded)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\r\nchanged\r\nthird\r\n" {
		t.Fatalf("file = %q", got)
	}
}

// A single-line old_string matches a CRLF file directly; new lines in
// new_string still get CRLF, and an edit that only changes line ends is a
// no-op.
func TestApplyEditKeepsCRLF(t *testing.T) {
	text := "first\r\nsecond\r\nthird\r\n"
	got, _, err := applyEdit("f", text, editArgs{OldString: "second", NewString: "second\ninserted"})
	if err != nil || got != "first\r\nsecond\r\ninserted\r\nthird\r\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	_, _, err = applyEdit("f", text, editArgs{OldString: "first\nsecond", NewString: "first\r\nsecond"})
	if err == nil {
		t.Fatal("CRLF-only change succeeded")
	}
	// A file with LF lines keeps them, even with one stray CRLF.
	got, _, err = applyEdit("f", "a\nb\r\nc\n", editArgs{OldString: "a", NewString: "a\nz"})
	if err != nil || got != "a\nz\nb\r\nc\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}
