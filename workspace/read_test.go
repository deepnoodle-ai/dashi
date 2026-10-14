package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestReadFileCutsLongLines(t *testing.T) {
	list, dir := newTestTools(t)
	long := strings.Repeat("x", 100_000)
	exact := strings.Repeat("y", readMaxLine)
	writeTestFile(t, dir, "min.js", "short\n"+long+"\n"+exact+"\r\nend")
	out, text := call(t, list, ToolReadFile, map[string]any{"path": "min.js"})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "     1\tshort\n")
	wantContains(t, text, fmt.Sprintf("[line cut at %d of 100001 bytes]", readMaxLine))
	wantContains(t, text, "     3\t"+exact+"\n")
	wantContains(t, text, "     4\tend\n")
	if len(text) > 3*readMaxLine+200 {
		t.Fatalf("result is %d bytes", len(text))
	}
}

func TestReadFileBoundsResultBytes(t *testing.T) {
	list, dir := newTestTools(t)
	line := strings.Repeat("z", 1000)
	writeTestFile(t, dir, "big.txt", strings.Repeat(line+"\n", 1000))
	out, text := call(t, list, ToolReadFile, map[string]any{"path": "big.txt"})
	wantState(t, out, text, tool.StateSucceeded)
	if len(text) > readMaxBytes+200 {
		t.Fatalf("result is %d bytes, bound is %d", len(text), readMaxBytes)
	}
	wantContains(t, text, "of 1000. Set offset to")
}

func TestReadFileStopsWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "hello\n")
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _ := readFile(ctx, s, readArgs{Path: "a.txt"})
	text := out.Output.Content[0].Text.Text
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "canceled")
}
