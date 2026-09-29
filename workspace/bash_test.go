package workspace

import (
	"context"
	"encoding/json"
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
