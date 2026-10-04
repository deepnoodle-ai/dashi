package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *testEnv) verify(snapshot string) verifyResult {
	e.t.Helper()
	res, err := runVerify(e.opts.ArchiveDir, snapshot)
	if err != nil {
		e.t.Fatalf("verify: %v", err)
	}
	return res
}

func TestVerifyClean(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	e.claude("paste-cache/p1.txt", "pasteX", baseTime.Add(time.Minute)) // supersedes
	e.copy()
	res := e.verify("")
	if res.problems() != 0 || res.Current != 16 || res.Superseded != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestVerifyHashMismatch(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	// Same length, different bytes.
	if err := os.WriteFile(e.archivePath("claude-code/paste-cache/p1.txt"), []byte("PASTED"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.verify("")
	if len(res.Mismatched) != 1 || res.Mismatched[0].Path != "claude-code/paste-cache/p1.txt" ||
		len(res.Missing) != 0 || len(res.Unreadable) != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestVerifyMissingCopy(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	e.claude("paste-cache/p1.txt", "pasteX", baseTime.Add(time.Minute))
	e.copy()
	old := eventsOf(e.events(), 0, eventCopySuperseded)[0].SupersededPath
	const cp = "codex/history.jsonl.gz"
	for _, p := range []string{cp, old} {
		if err := os.Remove(e.archivePath(p)); err != nil {
			t.Fatal(err)
		}
	}
	res := e.verify("")
	if len(res.Missing) != 2 || len(res.Mismatched) != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, p := range []string{cp, old} {
		if !strings.Contains(strings.Join(res.Missing, "\n"), p) {
			t.Errorf("missing list lacks %s", p)
		}
	}
	r := runCLI(t, "verify", "--archive", e.opts.ArchiveDir)
	if r.ExitCode != 1 {
		t.Fatalf("exit %d, want 1", r.ExitCode)
	}
	if !r.Contains("missing from disk: 2") || r.Err.Error() != "verify failed: 2 problems" {
		t.Fatalf("output:\n%s\n%v", r.Stdout, r.Err)
	}
}

// snapshot writes a file into a snapshot directory mirroring ~/.claude
// (claude/) and ~/.codex (codex/).
func (e *testEnv) snapshot(rel, content string) string {
	e.t.Helper()
	dir := filepath.Join(filepath.Dir(e.opts.ArchiveDir), "snapshot")
	e.write(dir, rel, content, baseTime)
	return dir
}

func TestVerifySnapshot(t *testing.T) {
	e := newEnv(t)
	const grown = "line 1\nline 2\n"
	e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession, baseTime)
	e.claude("projects/-work-demo/s-2.jsonl", grown, baseTime)
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.codex("sessions/2026/06/22/"+codexSessionName1, codexRootSession, baseTime)
	e.copy()
	// The paste changes after the snapshot; only the superseded copy
	// still has the snapshot's bytes.
	e.claude("paste-cache/p1.txt", "pasteX", baseTime.Add(time.Minute))
	e.copy()

	snap := e.snapshot("claude/projects/-work-demo/s-1.jsonl", claudeRootSession) // equal
	e.snapshot("claude/projects/-work-demo/s-2.jsonl", "line 1\n")                // the copy grew
	e.snapshot("claude/paste-cache/p1.txt", "pasted")                             // a superseded copy equals it
	// Codex archived the session after the snapshot was taken: it maps to
	// the same copy by the date in its name.
	e.snapshot("codex/archived_sessions/"+codexSessionName1, codexRootSession)
	e.snapshot("claude/history.jsonl", "differs\n")            // no copy at all
	e.snapshot("claude/projects/-work-demo/x.jsonl", "nope\n") // no copy at all
	e.snapshot("claude/settings.json", "{}")                   // not copied by copy
	e.snapshot("codex/state_5.sqlite", "db")                   // not copied by copy
	e.snapshot("codex/sessions/2026/06/22/.DS_Store", "x")     // not copied by copy

	res := e.verify(snap)
	s := res.Snapshot
	if res.problems() != 2 || s.Files != 6 || s.Equal != 3 || s.Grew != 1 || len(s.Failed) != 2 {
		t.Fatalf("result %+v, snapshot %+v", res, s)
	}
	failed := s.Failed[0].Path + " " + s.Failed[1].Path
	if !strings.Contains(failed, "claude/history.jsonl") || !strings.Contains(failed, "claude/projects/-work-demo/x.jsonl") {
		t.Fatalf("failed %+v", s.Failed)
	}
	want := []string{"claude/settings.json", "codex/sessions/2026/06/22/.DS_Store", "codex/state_5.sqlite"}
	if strings.Join(s.NotCopied, ",") != strings.Join(want, ",") {
		t.Fatalf("not copied %v, want %v", s.NotCopied, want)
	}
	// Nothing in the snapshot was touched.
	if b, err := os.ReadFile(filepath.Join(snap, "claude", "history.jsonl")); err != nil || string(b) != "differs\n" {
		t.Fatal("snapshot file changed")
	}
}

func TestVerifySnapshotRejectsDifferentBytes(t *testing.T) {
	e := newEnv(t)
	e.claude("projects/-work-demo/s-1.jsonl", "line 1\nline 2\n", baseTime)
	e.copy()
	cases := map[string]string{
		"same size":   "LINE 1\nline 2\n",
		"shorter":     "line 2\n",
		"longer":      "line 1\nline 2\nline 3\n",
		"not a start": "ine 1\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			snap := e.snapshot("claude/projects/-work-demo/s-1.jsonl", content)
			s := e.verify(snap).Snapshot
			if len(s.Failed) != 1 || !strings.Contains(s.Failed[0].Reason, "equals or begins with") {
				t.Fatalf("snapshot %+v", s)
			}
		})
	}
}

func TestVerifySnapshotCommandLine(t *testing.T) {
	e := newEnv(t)
	e.claude("history.jsonl", "a\nb\n", baseTime)
	e.copy()
	snap := e.snapshot("claude/history.jsonl", "a\n")
	args := []string{"verify", "--archive", e.opts.ArchiveDir, "--snapshot", snap}
	r := runCLI(t, args...)
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %v\n%s", r.ExitCode, r.Err, r.Stdout)
	}
	if !r.Contains("accepted: 1 (0 equal a copy, 1 are the start of a longer copy)") ||
		!r.Contains("verify passed") {
		t.Fatalf("output:\n%s", r.Stdout)
	}
	e.snapshot("claude/history.jsonl", "c\n")
	if r = runCLI(t, args...); r.ExitCode != 1 {
		t.Fatalf("exit %d, want 1", r.ExitCode)
	}
	if !r.Contains("failed, keep these: 1") || r.Contains("verify passed") {
		t.Fatalf("output:\n%s", r.Stdout)
	}
	empty := t.TempDir()
	if r := runCLI(t, "verify", "--archive", e.opts.ArchiveDir, "--snapshot", empty); r.ExitCode != 1 {
		t.Fatalf("snapshot without claude/ or codex/: exit %d", r.ExitCode)
	}
}

func TestVerifyNeedsAnArchive(t *testing.T) {
	r := runCLI(t, "verify", "--archive", t.TempDir())
	if r.ExitCode != 1 || !strings.Contains(r.Err.Error(), "no database") {
		t.Fatalf("exit %d: %v", r.ExitCode, r.Err)
	}
}
