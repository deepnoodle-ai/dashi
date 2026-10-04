package main

import (
	"bytes"
	"compress/gzip"
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

func gzipBytes(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestRecoveryLogsUnknownCopy(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/projects/p/s.jsonl.gz"
	e.claude("projects/p/other.jsonl", claudeRootSession, baseTime)
	e.copy()
	// A crash after renaming a first copy into place, before logging it.
	e.claude("projects/p/s.jsonl", claudeSubagent, baseTime)
	full := e.archivePath(cp)
	os.WriteFile(full, gzipBytes(claudeSubagent), 0o600)
	e.touch(full, baseTime)
	n := len(e.events())
	sum := e.copy()
	written := eventsOf(e.events(), n, eventCopyWritten)
	if len(written) != 1 || written[0].CopyPath != cp || !written[0].Recovered || written[0].Hash != sha(claudeSubagent) {
		t.Fatalf("recovered events: %+v", written)
	}
	if sum.OriginalsCopied != 0 {
		t.Fatal("the original was copied again after recovery")
	}
	row, _ := e.copyRowAt(cp)
	if row.OriginalPath == "" || row.OriginalBytes != int64(len(claudeSubagent)) {
		t.Fatalf("recovered row: %+v", row)
	}
}

func TestRecoveryLogsRenamedOldCopy(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	// A crash mid-supersede: the old copy was renamed and the new copy put
	// in its place, but neither was logged.
	renamedAt := time.Date(2026, 9, 3, 4, 5, 6, 0, time.UTC)
	old := supersededPath(cp, renamedAt, 0)
	if err := os.Rename(e.archivePath(cp), e.archivePath(old)); err != nil {
		t.Fatal(err)
	}
	mtime := baseTime.Add(time.Hour)
	e.claude("paste-cache/p1.txt", "PASTED", mtime)
	os.WriteFile(e.archivePath(cp), []byte("PASTED"), 0o600)
	e.touch(e.archivePath(cp), mtime)
	n := len(e.events())
	sum := e.copy()
	evs := e.events()
	sup := eventsOf(evs, n, eventCopySuperseded)
	if len(sup) != 1 || sup[0].CopyPath != cp || sup[0].SupersededPath != old || !sup[0].Recovered || !sup[0].Time.Equal(renamedAt) {
		t.Fatalf("superseded events: %+v", sup)
	}
	written := eventsOf(evs, n, eventCopyWritten)
	if len(written) != 1 || !written[0].Recovered || written[0].Hash != sha("PASTED") {
		t.Fatalf("written events: %+v", written)
	}
	if sum.OriginalsCopied != 0 {
		t.Fatal("the original was copied again after recovery")
	}
	oldRow, oldSup := e.copyRowAt(old)
	if !oldSup.Valid || oldRow.Hash != sha("pasted") {
		t.Fatal("the renamed old copy is not recorded as superseded")
	}
}

func TestRecoveryAfterRenameOfOldCopyOnly(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	old := supersededPath(cp, time.Now(), 0)
	os.Rename(e.archivePath(cp), e.archivePath(old))
	e.claude("paste-cache/p1.txt", "PASTED", baseTime.Add(time.Hour))
	sum := e.copy()
	if sum.OriginalsCopied != 1 {
		t.Fatalf("copied %d, want 1", sum.OriginalsCopied)
	}
	if e.readCopy(cp) != "PASTED" || e.readCopy(old) != "pasted" {
		t.Fatal("copies wrong after recovery")
	}
	if e.countCopies("superseded_at IS NOT NULL") != 1 || e.countCopies("superseded_at IS NULL") != 1 {
		t.Fatal("want one superseded and one current copy")
	}
}
