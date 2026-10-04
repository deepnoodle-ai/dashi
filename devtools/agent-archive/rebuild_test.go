package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRebuildEqualsIncremental(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	// A second run with every kind of change: append, rewrite, time only,
	// a Codex move, and a deleted original.
	e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession+`{"type":"system"}`+"\n", baseTime.Add(time.Minute))
	e.claude("paste-cache/p1.txt", "changed", baseTime.Add(time.Minute))
	e.touch(filepath.Join(e.opts.ClaudeDir, "history.jsonl"), baseTime.Add(time.Hour))
	os.Remove(filepath.Join(e.opts.ClaudeDir, "file-history", "s-1", "abc@v1"))
	os.MkdirAll(filepath.Join(e.opts.CodexDir, "archived_sessions"), 0o700)
	os.Rename(filepath.Join(e.opts.CodexDir, "sessions", "2026", "06", "22", codexSessionName1),
		filepath.Join(e.opts.CodexDir, "archived_sessions", codexSessionName1))
	e.copy()
	e.copy()
	want := e.dump()
	if !strings.Contains(want, "superseded-") || !strings.Contains(want, `"user":1`) {
		t.Fatalf("fixture did not exercise supersede and parsing:\n%s", want)
	}

	sum, err := runRebuild(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Events != len(e.events()) {
		t.Fatalf("rebuild applied %d of %d events", sum.Events, len(e.events()))
	}
	if got := e.dump(); got != want {
		t.Fatalf("rebuild differs from incremental:\n got:\n%s\nwant:\n%s", got, want)
	}
	// The archive still works after a rebuild.
	if sum := e.copy(); sum.OriginalsCopied != 0 {
		t.Fatal("copied after a rebuild with no changes")
	}
	if _, err := os.Stat(filepath.Join(e.opts.ArchiveDir, databaseName+".rebuild")); !os.IsNotExist(err) {
		t.Fatal("rebuild left its temporary database")
	}
}

func TestRebuildReparsesWithCurrentParser(t *testing.T) {
	e := newEnv(t)
	e.claude("projects/p/s.jsonl", claudeRootSession, baseTime)
	e.copy()
	db := e.db()
	if _, err := db.Exec(`UPDATE copies SET line_types = '{}', parser_version = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := runRebuild(e.opts.ArchiveDir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.dump(), `"assistant":1`) {
		t.Fatal("rebuild did not parse the copies again")
	}
}
