package workspace

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestBashRunsInWorkspaceAndMergesOutput(t *testing.T) {
	list, dir := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{
		"command": "pwd; echo out; echo err >&2; echo out2",
	})
	wantState(t, out, text, tool.StateSucceeded)
	resolved, _ := filepath.EvalSymlinks(dir)
	if text != resolved+"\nout\nerr\nout2" {
		t.Fatalf("text = %q", text)
	}
}

func TestBashNonZeroExitFails(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{"command": "echo failing; exit 3"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "failing")
	wantContains(t, text, "Exit code 3")
}

func TestBashInputIsEmpty(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{"command": "cat; echo done"})
	wantState(t, out, text, tool.StateSucceeded)
	if text != "done" {
		t.Fatalf("text = %q", text)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	list, dir := newTestTools(t)
	marker := filepath.Join(dir, "child-survived")
	// The background child would create the marker if the kill missed it.
	command := "(sleep 2; touch " + marker + ") & echo started; sleep 30"
	start := time.Now()
	out, text := call(t, list, ToolBash, map[string]any{"command": command, "timeout": 300})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "started")
	wantContains(t, text, "ran past the timeout")
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the call took %s", time.Since(start))
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child process outlived the timeout")
	}
}

func TestBashClampsTimeoutAndSaysSo(t *testing.T) {
	list, _ := newTestTools(t, WithShell(ShellConfig{MaxTimeout: time.Second}))
	out, text := call(t, list, ToolBash, map[string]any{"command": "echo hi", "timeout": 60000})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "reduced to the maximum, 1s")
}

func TestBashClampsHugeTimeout(t *testing.T) {
	list, _ := newTestTools(t, WithShell(ShellConfig{MaxTimeout: time.Second}))
	out, text := call(t, list, ToolBash, map[string]any{"command": "echo hi", "timeout": math.MaxInt64})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "reduced to the maximum, 1s")
	// With no configured maximum the default applies; the value still must
	// not wrap to a negative duration.
	list, _ = newTestTools(t)
	out, text = call(t, list, ToolBash, map[string]any{"command": "echo hi", "timeout": math.MaxInt64})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "reduced to the maximum")
}

func TestBashCancelStopsCommand(t *testing.T) {
	list, _ := newTestTools(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	out, text := callCtx(t, ctx, list, ToolBash, map[string]any{"command": "sleep 30"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "canceled")
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel did not stop the command")
	}
}

func TestBashReportsProgress(t *testing.T) {
	list, _ := newTestTools(t)
	var mu sync.Mutex
	var reports []tool.Progress
	ctx := tool.WithReporter(context.Background(), func(p tool.Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, p)
	})
	command := "echo first; sleep 0.4; echo second; sleep 0.4; echo third"
	out, text := callCtx(t, ctx, list, ToolBash, map[string]any{"command": command})
	wantState(t, out, text, tool.StateSucceeded)
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 || len(reports) > 8 {
		t.Fatalf("got %d reports", len(reports))
	}
	var data ShellProgress
	if err := json.Unmarshal(reports[len(reports)-1].Data, &data); err != nil {
		t.Fatal(err)
	}
	last := reports[len(reports)-1]
	if last.Message != data.Lines[len(data.Lines)-1] || !strings.Contains(last.Message, "third") {
		t.Fatalf("last report = %+v, lines = %v", last, data.Lines)
	}
}

func TestBashRefusesEmptyCommand(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{"command": "  "})
	wantState(t, out, text, tool.StateFailed)
}

func TestBashBackgroundProcessDoesNotFailTheCommand(t *testing.T) {
	list, _ := newTestTools(t)
	start := time.Now()
	out, text := call(t, list, ToolBash, map[string]any{"command": "sleep 5 & echo started"})
	wantState(t, out, text, tool.StateSucceeded)
	wantContains(t, text, "started")
	wantContains(t, text, "background process")
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the call took %s", time.Since(start))
	}
}

func TestBashBackgroundProcessWithRedirectedOutputHasNoNote(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{"command": "sleep 5 >/dev/null 2>&1 & echo started"})
	wantState(t, out, text, tool.StateSucceeded)
	if text != "started" {
		t.Fatalf("text = %q", text)
	}
}

func TestBashBackgroundProcessKeepsRealFailure(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{"command": "sleep 5 & echo bad; exit 2"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "Exit code 2")
}

func TestBashOutputKeepsHeadAndTailWithinLimit(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolBash, map[string]any{
		"command": "echo FIRST; yes abcdefghi | head -c 5000000; echo; echo LAST",
	})
	wantState(t, out, text, tool.StateSucceeded)
	if len(text) > shellHeadMax+shellTailMax+1024 {
		t.Fatalf("result has %d bytes", len(text))
	}
	if !strings.HasPrefix(text, "FIRST\n") || !strings.HasSuffix(text, "LAST") {
		t.Fatalf("head or tail missing: %q ... %q", text[:20], text[len(text)-20:])
	}
	wantContains(t, text, "bytes of output dropped from the middle")
}

func TestShellOutputCounts(t *testing.T) {
	o := newShellOutput(context.Background(), 4, 4)
	for _, chunk := range []string{"ab", "cdef", "ghij", "klmn", "opqr"} {
		o.Write([]byte(chunk))
	}
	got := o.String()
	// 18 bytes in all: head "abcd", tail "opqr", 10 dropped.
	wantContains(t, got, "abcd\n[... 10 bytes of output dropped")
	if !strings.HasSuffix(got, "...]\nopqr") {
		t.Fatalf("output = %q", got)
	}
	small := newShellOutput(context.Background(), 8, 8)
	small.Write([]byte("hello\n"))
	if got := small.String(); got != "hello" {
		t.Fatalf("small = %q", got)
	}
}

func TestBashDefaultTimeoutRespectsMax(t *testing.T) {
	list, _ := newTestTools(t, WithShell(ShellConfig{MaxTimeout: 200 * time.Millisecond}))
	start := time.Now()
	out, text := call(t, list, ToolBash, map[string]any{"command": "sleep 5"})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "timeout of 200ms")
	if time.Since(start) > 4*time.Second {
		t.Fatal("the default timeout was not capped by MaxTimeout")
	}
}
