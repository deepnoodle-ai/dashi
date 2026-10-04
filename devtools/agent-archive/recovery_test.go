package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendToLog(t *testing.T, e *testEnv, raw string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(e.opts.ArchiveDir, copyLogName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDropsTornLastLine(t *testing.T) {
	e := newEnv(t)
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	logPath := filepath.Join(e.opts.ArchiveDir, copyLogName)
	good, _ := os.ReadFile(logPath)
	appendToLog(t, e, `{"event":"copy_written","time":"2026-`)
	sum := e.copy()
	if !strings.Contains(strings.Join(sum.Warnings, "\n"), "torn last line") {
		t.Fatalf("no torn-line warning: %v", sum.Warnings)
	}
	after, _ := os.ReadFile(logPath)
	if !bytes.HasPrefix(after, good) || bytes.Contains(after, []byte(`"time":"2026-{`)) {
		t.Fatal("torn line not dropped cleanly")
	}
	if _, _, err := readLog(logPath, 0); err != nil {
		t.Fatalf("log not valid after recovery: %v", err)
	}
}

func TestReadLogRejectsABadMiddleLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), copyLogName)
	os.WriteFile(p, []byte("{\"event\":\"run_started\",\"time\":\"2026-10-03T00:00:00Z\",\"run_id\":\"r\"}\nnot json\n{\"event\":\"run_started\",\"time\":\"2026-10-03T00:00:00Z\",\"run_id\":\"s\"}\n"), 0o600)
	if _, _, err := readLog(p, 0); err == nil {
		t.Fatal("a bad line in the middle of the log should be an error")
	}
}

func TestRecoveryAppliesUnappliedEvents(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	// A crash after appending an event and before updating SQLite.
	goneAt := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	line, _ := encodeEvent(logEvent{Event: eventOriginalGone, Time: goneAt, RunID: "crashed", CopyPath: cp})
	appendToLog(t, e, string(line))
	a, err := openArchive(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	row, _ := e.copyRowAt(cp)
	if row.OriginalGoneAt.String != formatTime(goneAt) {
		t.Fatalf("unapplied event not applied: %v", row.OriginalGoneAt)
	}
}

func TestRecoveryFromLogAloneMatchesDatabase(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	want := e.dump()
	removeDatabase(filepath.Join(e.opts.ArchiveDir, databaseName))
	a, err := openArchive(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsePending(a.DB, a.Dir); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if got := e.dump(); got != want {
		t.Fatalf("database applied from the log differs:\n got %s\nwant %s", got, want)
	}
}

func TestRecoveryDeletesLeftoverTempFile(t *testing.T) {
	e := newEnv(t)
	e.claude("projects/p/s.jsonl", claudeRootSession, baseTime)
	e.copy()
	tmp := filepath.Join(e.opts.ArchiveDir, "claude-code", "projects", "p", tempPrefix+"12345")
	os.WriteFile(tmp, []byte("partial"), 0o600)
	sum := e.copy()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("leftover temporary file not deleted")
	}
	if !strings.Contains(strings.Join(sum.Warnings, "\n"), "temporary file") {
		t.Fatalf("warnings %v", sum.Warnings)
	}
}
