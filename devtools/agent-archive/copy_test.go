package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const codexSessionName1 = "rollout-2026-06-22T15-30-25-0000aaaa-0000-0000-0000-000000000001.jsonl"

// writeEverything writes one of each kind of original, plus files that
// must not be copied.
func writeEverything(e *testEnv) {
	t := baseTime
	e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession, t)
	e.claude("projects/-work-demo/s-1/subagents/agent-a-7.jsonl", claudeSubagent, t)
	e.claude("projects/-work-demo/s-1/subagents/agent-a-7.meta.json", `{"agentType":"explore"}`, t)
	e.claude("projects/-work-demo/s-1/tool-results/r1.txt", "tool output", t)
	e.claude("projects/-work-demo/memory/MEMORY.md", "# memory", t)
	e.claude("projects/-work-demo/sessions-index.json", "{}", t)
	e.claude("history.jsonl", `{"display":"hello"}`+"\n", t)
	e.claude("file-history/s-1/abc@v1", "old file", t)
	e.claude("paste-cache/p1.txt", "pasted", t)
	e.claude("settings.json", "{}", t) // not copied: outside what is copied

	e.codex("sessions/2026/06/22/"+codexSessionName1, codexRootSession, t)
	e.codex("archived_sessions/rollout-2026-05-19T07-25-26-0000aaaa-0000-0000-0000-000000000002.jsonl", codexSubagentSession, t)
	e.codex("session_index.jsonl", `{"id":"c-1","thread_name":"demo"}`+"\n", t)
	e.codex("history.jsonl", `{"text":"hi"}`+"\n", t)
	e.codex("attachments/x/a.txt", "attached", t)
	e.codex("generated_images/i.png", "png", t)
	e.codex("memories/MEMORY.md", "# codex memory", t)
	e.codex("memories/index.sqlite", "not copied", t)
	e.codex("state_5.sqlite", "not copied", t)
	e.codex("logs_2.sqlite-wal", "not copied", t)
	e.codex("notes/n.md", "not copied", t)
}

func TestFirstCopy(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	sum := e.copy()
	if sum.OriginalsSeen != 16 || sum.OriginalsCopied != 16 {
		t.Fatalf("seen %d copied %d, want 16 and 16; warnings %v", sum.OriginalsSeen, sum.OriginalsCopied, sum.Warnings)
	}
	want := map[string]string{
		"claude-code/projects/-work-demo/s-1.jsonl.gz":                                                        kindSession,
		"claude-code/projects/-work-demo/s-1/subagents/agent-a-7.jsonl.gz":                                    kindSubagent,
		"claude-code/projects/-work-demo/s-1/subagents/agent-a-7.meta.json":                                   kindSubagentMeta,
		"claude-code/projects/-work-demo/s-1/tool-results/r1.txt":                                             kindToolResult,
		"claude-code/projects/-work-demo/memory/MEMORY.md":                                                    kindMemory,
		"claude-code/projects/-work-demo/sessions-index.json":                                                 kindOther,
		"claude-code/history.jsonl.gz":                                                                        kindPromptLog,
		"claude-code/file-history/s-1/abc@v1":                                                                 kindFileHistory,
		"claude-code/paste-cache/p1.txt":                                                                      kindPaste,
		"codex/sessions/2026/06/22/" + codexSessionName1 + ".gz":                                              kindSession,
		"codex/sessions/2026/05/19/rollout-2026-05-19T07-25-26-0000aaaa-0000-0000-0000-000000000002.jsonl.gz": kindSession,
		"codex/session_index.jsonl.gz":                                                                        kindSessionTitles,
		"codex/history.jsonl.gz":                                                                              kindPromptLog,
		"codex/attachments/x/a.txt":                                                                           kindAttachment,
		"codex/generated_images/i.png":                                                                        kindGeneratedImage,
		"codex/memories/MEMORY.md":                                                                            kindMemory,
	}
	written := eventsOf(e.events(), 0, eventCopyWritten)
	if len(written) != len(want) {
		t.Fatalf("got %d copy_written events, want %d", len(written), len(want))
	}
	for _, ev := range written {
		kind, ok := want[ev.CopyPath]
		if !ok {
			t.Errorf("unexpected copy %s", ev.CopyPath)
			continue
		}
		if ev.Kind != kind {
			t.Errorf("%s: kind %s, want %s", ev.CopyPath, ev.Kind, kind)
		}
		if ev.OriginalBytes == nil || ev.ModifiedAt == nil || ev.Hash == "" || ev.App == "" || ev.OriginalPath == "" || ev.RunID == "" {
			t.Errorf("%s: copy_written lacks a field: %+v", ev.CopyPath, ev)
		}
		if !ev.ModifiedAt.Equal(baseTime) {
			t.Errorf("%s: modified_at %v", ev.CopyPath, ev.ModifiedAt)
		}
		if gz := isGzipFile(t, e.archivePath(ev.CopyPath)); gz != strings.HasSuffix(ev.CopyPath, ".jsonl.gz") {
			t.Errorf("%s: gzipped %v", ev.CopyPath, gz)
		}
		orig, err := os.ReadFile(ev.OriginalPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.readCopy(ev.CopyPath); got != string(orig) {
			t.Errorf("%s: copy differs from original", ev.CopyPath)
		}
		if ev.Hash != sha(string(orig)) {
			t.Errorf("%s: hash is not the SHA-256 of the original", ev.CopyPath)
		}
	}
	evs := e.events()
	if evs[0].Event != eventRunStarted || evs[len(evs)-1].Event != eventRunEnded {
		t.Fatalf("log should start with run_started and end with run_ended")
	}
	if n := e.countCopies("copy_path LIKE '%notes%' OR copy_path LIKE '%sqlite%' OR copy_path LIKE '%settings%'"); n != 0 {
		t.Fatalf("copied %d files that must not be copied", n)
	}
	// Copies carry their original's modified time.
	info, err := os.Stat(e.archivePath("claude-code/paste-cache/p1.txt"))
	if err != nil || !info.ModTime().Equal(baseTime) {
		t.Fatalf("copy modified time %v, want %v", info.ModTime(), baseTime)
	}
}

func TestSkipWhenSizeAndModifiedTimeMatch(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	e.copy()
	n := len(e.events())
	sum := e.copy()
	if sum.OriginalsCopied != 0 || sum.ModifiedAt != 0 {
		t.Fatalf("second run copied %d, updated %d; want 0", sum.OriginalsCopied, sum.ModifiedAt)
	}
	for _, ev := range e.events()[n:] {
		if ev.Event != eventRunStarted && ev.Event != eventRunEnded {
			t.Fatalf("unexpected %s event on an unchanged run", ev.Event)
		}
	}
}

func TestOnlyModifiedTimeChanged(t *testing.T) {
	e := newEnv(t)
	p := e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	n := len(e.events())
	later := baseTime.Add(time.Hour)
	e.touch(p, later)
	sum := e.copy()
	if sum.ModifiedAt != 1 || sum.OriginalsCopied != 0 {
		t.Fatalf("modified-time updates %d, copied %d; want 1 and 0", sum.ModifiedAt, sum.OriginalsCopied)
	}
	evs := e.events()
	if got := eventsOf(evs, n, eventModifiedAtUpdated); len(got) != 1 || !got[0].ModifiedAt.Equal(later) {
		t.Fatalf("modified_at_updated events: %+v", got)
	}
	if got := eventsOf(evs, n, eventCopyWritten); len(got) != 0 {
		t.Fatalf("wrote a new copy when only the time changed")
	}
	row, _ := e.copyRowAt("claude-code/paste-cache/p1.txt")
	if row.ModifiedAt != formatTime(later) {
		t.Fatalf("database modified_at %s", row.ModifiedAt)
	}
	if e.countCopies("1=1") != 1 {
		t.Fatal("want exactly one copy")
	}
	// A third run skips it.
	if sum := e.copy(); sum.ModifiedAt != 0 {
		t.Fatal("third run updated the modified time again")
	}
}

func TestAppendedOriginalRewritesCopy(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/projects/-work-demo/s-1.jsonl.gz"
	e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession, baseTime)
	e.copy()
	before, _ := e.copyRowAt(cp)
	n := len(e.events())
	grown := claudeRootSession + `{"type":"user","sessionId":"s-1"}` + "\n"
	e.claude("projects/-work-demo/s-1.jsonl", grown, baseTime.Add(time.Minute))
	sum := e.copy()
	if sum.OriginalsCopied != 1 || sum.Superseded != 0 {
		t.Fatalf("copied %d superseded %d; want 1 and 0", sum.OriginalsCopied, sum.Superseded)
	}
	if got := eventsOf(e.events(), n, eventCopySuperseded); len(got) != 0 {
		t.Fatal("an appended original must not supersede its copy")
	}
	after, superseded := e.copyRowAt(cp)
	if after.CopyID != before.CopyID || superseded.Valid {
		t.Fatalf("the copy should be rewritten in place: before %d after %d", before.CopyID, after.CopyID)
	}
	if after.Hash != sha(grown) || after.OriginalBytes != int64(len(grown)) {
		t.Fatal("database does not describe the rewritten copy")
	}
	if e.readCopy(cp) != grown {
		t.Fatal("copy was not rewritten")
	}
	if e.countCopies("1=1") != 1 {
		t.Fatal("want exactly one copy")
	}
}

func TestChangedOriginalSupersedesCopy(t *testing.T) {
	cases := map[string]string{
		"shrunk":                 "short\n",
		"same size, changed":     "pasteX",
		"larger, prefix differs": "PASTED and more",
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			const cp = "claude-code/paste-cache/p1.txt"
			e.claude("paste-cache/p1.txt", "pasted", baseTime)
			e.copy()
			n := len(e.events())
			e.claude("paste-cache/p1.txt", next, baseTime.Add(time.Minute))
			sum := e.copy()
			if sum.OriginalsCopied != 1 || sum.Superseded != 1 {
				t.Fatalf("copied %d superseded %d; want 1 and 1", sum.OriginalsCopied, sum.Superseded)
			}
			evs := e.events()
			sup := eventsOf(evs, n, eventCopySuperseded)
			if len(sup) != 1 || sup[0].CopyPath != cp {
				t.Fatalf("copy_superseded events: %+v", sup)
			}
			old := sup[0].SupersededPath
			if !strings.HasPrefix(old, "claude-code/paste-cache/p1.superseded-") || !strings.HasSuffix(old, ".txt") {
				t.Fatalf("superseded name %s", old)
			}
			if e.readCopy(old) != "pasted" {
				t.Fatal("superseded copy is not the old content")
			}
			if e.readCopy(cp) != next {
				t.Fatal("new copy is not the new content")
			}
			oldRow, oldSup := e.copyRowAt(old)
			newRow, newSup := e.copyRowAt(cp)
			if !oldSup.Valid || newSup.Valid || oldRow.Hash != sha("pasted") || newRow.Hash != sha(next) {
				t.Fatal("database rows do not match the copies")
			}
			// Nothing changes on the next run.
			if sum := e.copy(); sum.OriginalsCopied != 0 {
				t.Fatal("copied again on an unchanged run")
			}
		})
	}
}

func TestCopiesTheSizeReadAtStart(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/projects/-work-demo/s-1.jsonl.gz"
	p := e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession, baseTime)
	listed := listOriginals(appClaudeCode, e.opts.ClaudeDir).Originals
	if len(listed) != 1 {
		t.Fatalf("listed %d originals", len(listed))
	}
	// The app appends while the copy is under way.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"user","sessi`)
	f.Close()

	a, err := openArchive(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.copyOriginal(listed[0], nil); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if got := e.readCopy(cp); got != claudeRootSession {
		t.Fatalf("copy is not the clean prefix read at the start:\n%q", got)
	}
	row, _ := e.copyRowAt(cp)
	if row.Hash != sha(claudeRootSession) || row.OriginalBytes != int64(len(claudeRootSession)) {
		t.Fatal("database does not describe the prefix")
	}
	// The next run sees an appended original and rewrites the copy.
	e.touch(p, baseTime.Add(time.Minute))
	sum := e.copy()
	if sum.OriginalsCopied != 1 || sum.Superseded != 0 {
		t.Fatalf("copied %d superseded %d; want 1 and 0", sum.OriginalsCopied, sum.Superseded)
	}
}

func TestCodexFileMovedToArchivedSessions(t *testing.T) {
	e := newEnv(t)
	p := e.codex("sessions/2026/06/22/"+codexSessionName1, codexRootSession, baseTime)
	e.copy()
	n := len(e.events())
	dst := filepath.Join(e.opts.CodexDir, "archived_sessions", codexSessionName1)
	os.MkdirAll(filepath.Dir(dst), 0o700)
	if err := os.Rename(p, dst); err != nil {
		t.Fatal(err)
	}
	sum := e.copy()
	if sum.OriginalsCopied != 0 || sum.Gone != 0 {
		t.Fatalf("copied %d gone %d after a move; want 0 and 0", sum.OriginalsCopied, sum.Gone)
	}
	for _, ev := range e.events()[n:] {
		if ev.Event != eventRunStarted && ev.Event != eventRunEnded {
			t.Fatalf("unexpected %s event after a move", ev.Event)
		}
	}
	if e.countCopies("1=1") != 1 {
		t.Fatal("a moved Codex file made a second copy")
	}
}

func TestOriginalGone(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	p := e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.claude("paste-cache/p2.txt", "kept", baseTime)
	e.copy()
	os.Remove(p)
	if sum := e.copy(); sum.Gone != 1 {
		t.Fatalf("gone %d, want 1", sum.Gone)
	}
	row, _ := e.copyRowAt(cp)
	if !row.OriginalGoneAt.Valid {
		t.Fatal("original_gone_at not set")
	}
	if e.readCopy(cp) != "pasted" {
		t.Fatal("copy was not kept")
	}
	n := len(e.events())
	e.copy()
	if got := eventsOf(e.events(), n, eventOriginalGone); len(got) != 0 {
		t.Fatal("original_gone logged twice")
	}
}

func TestOriginalReturnsUnchanged(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	p := e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	os.Remove(p)
	e.copy()
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	n := len(e.events())
	if sum := e.copy(); sum.OriginalsCopied != 0 {
		t.Fatalf("copied %d after an unchanged return, want 0", sum.OriginalsCopied)
	}
	if got := eventsOf(e.events(), n, eventOriginalReturned); len(got) != 1 {
		t.Fatalf("%d original_returned events, want 1", len(got))
	}
	if row, _ := e.copyRowAt(cp); row.OriginalGoneAt.Valid {
		t.Fatal("original_gone_at not cleared")
	}
	os.Remove(p)
	if sum := e.copy(); sum.Gone != 1 {
		t.Fatalf("gone %d after a second removal, want 1", sum.Gone)
	}
}

func TestMissingAppDirectoryMarksNothingGone(t *testing.T) {
	e := newEnv(t)
	e.codex("history.jsonl", "{}\n", baseTime)
	e.copy()
	os.RemoveAll(e.opts.CodexDir)
	sum := e.copy()
	if sum.Gone != 0 {
		t.Fatal("a missing app directory marked its copies gone")
	}
	if !strings.Contains(strings.Join(sum.Warnings, "\n"), "no directory") {
		t.Fatalf("warnings %v", sum.Warnings)
	}
}

func TestGzipRoundTripIsByteExact(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := range 3000 {
		b.WriteByte(byte(i * 7))
	}
	content := "{\"type\":\"x\"}\r\n\x00\xff" + b.String() // no trailing newline
	e.claude("history.jsonl", content, baseTime)
	e.copy()
	if got := e.readCopy("claude-code/history.jsonl.gz"); got != content {
		t.Fatal("gzip round trip changed the bytes")
	}
}

func TestPermissionsFixed(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.opts.ArchiveDir, "snapshots", "2026-10-03"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(e.opts.ArchiveDir, 0o755)
	loose := filepath.Join(e.opts.ArchiveDir, "snapshots", "2026-10-03", "x.jsonl")
	os.WriteFile(loose, []byte("{}\n"), 0o644)
	os.Chmod(loose, 0o644)
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	sum := e.copy()
	if !strings.Contains(strings.Join(sum.Warnings, "\n"), "fixed permissions") {
		t.Fatalf("no permissions warning: %v", sum.Warnings)
	}
	err := filepath.Walk(e.opts.ArchiveDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v", p, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum := e.copy(); len(sum.Warnings) != 0 {
		t.Fatalf("second run warned: %v", sum.Warnings)
	}
}

func TestSnapshotsAreNotCopies(t *testing.T) {
	e := newEnv(t)
	snap := filepath.Join(e.opts.ArchiveDir, "snapshots", "2026-10-03", "claude", "projects", "p", "s.jsonl")
	os.MkdirAll(filepath.Dir(snap), 0o700)
	os.WriteFile(snap, []byte("{}\n"), 0o600)
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	sum := e.copy()
	if len(sum.Warnings) != 0 {
		t.Fatalf("warnings: %v", sum.Warnings)
	}
	if n := e.countCopies("copy_path LIKE 'snapshots/%'"); n != 0 {
		t.Fatal("a snapshot file was treated as a copy")
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatal("snapshot file was touched")
	}
}

func TestSecondRunIsLocked(t *testing.T) {
	e := newEnv(t)
	a, err := openArchive(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := runCopy(e.opts); err == nil || !strings.Contains(err.Error(), "another") {
		t.Fatalf("second run: %v", err)
	}
}

// A supersede that stops before the new copy takes the old name leaves the
// log and the disk consistent: the old copy is tracked under its superseded
// name and the next run writes a fresh copy.
func TestInterruptedSupersedeRecovers(t *testing.T) {
	e := newEnv(t)
	const cp = "claude-code/paste-cache/p1.txt"
	e.claude("paste-cache/p1.txt", "pasted", baseTime)
	e.copy()
	e.claude("paste-cache/p1.txt", "pasteX", baseTime.Add(time.Minute))
	renameIntoPlace = func(string, string) error { return os.ErrPermission }
	sum := e.copy()
	renameIntoPlace = os.Rename
	if len(sum.Warnings) != 1 {
		t.Fatalf("warnings: %v", sum.Warnings)
	}
	if sum := e.copy(); sum.OriginalsCopied != 1 {
		t.Fatalf("copied %d; want 1", sum.OriginalsCopied)
	}
	if e.readCopy(cp) != "pasteX" {
		t.Fatal("new copy is not the new content")
	}
	res := e.verify("")
	if res.problems() != 0 || res.Current != 1 || res.Superseded != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestMissingNewestCopyIsCopiedAgain(t *testing.T) {
	cases := map[string]struct {
		next  string
		mtime time.Time
	}{
		"original unchanged": {claudeRootSession, baseTime},
		"original changed":   {strings.Replace(claudeRootSession, "hello", "HELLO", 1), baseTime.Add(time.Minute)},
		"original grew":      {claudeRootSession + `{"type":"user"}` + "\n", baseTime.Add(time.Minute)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			const cp = "claude-code/projects/-work-demo/s-1.jsonl.gz"
			e.claude("projects/-work-demo/s-1.jsonl", claudeRootSession, baseTime)
			e.copy()
			if err := os.Remove(e.archivePath(cp)); err != nil {
				t.Fatal(err)
			}
			n := len(e.events())
			e.claude("projects/-work-demo/s-1.jsonl", c.next, c.mtime)
			sum := e.copy()
			if sum.OriginalsCopied != 1 || sum.Superseded != 0 {
				t.Fatalf("copied %d superseded %d; want 1 and 0", sum.OriginalsCopied, sum.Superseded)
			}
			if len(sum.Warnings) != 1 || !strings.Contains(sum.Warnings[0], "missing") || !strings.Contains(sum.Warnings[0], cp) {
				t.Fatalf("warnings: %v", sum.Warnings)
			}
			evs := e.events()
			written := eventsOf(evs, n, eventCopyWritten)
			if len(written) != 1 || written[0].CopyPath != cp {
				t.Fatalf("copy_written events: %+v", written)
			}
			if len(eventsOf(evs, n, eventCopySuperseded)) != 0 {
				t.Fatal("a missing copy was superseded")
			}
			if e.readCopy(cp) != c.next {
				t.Fatal("the fresh copy is not the original")
			}
			if row, _ := e.copyRowAt(cp); row.Hash != sha(c.next) || e.countCopies("1=1") != 1 {
				t.Fatal("database does not match the fresh copy")
			}
			if sum := e.copy(); sum.OriginalsCopied != 0 || len(sum.Warnings) != 0 {
				t.Fatalf("next run copied %d, warnings %v", sum.OriginalsCopied, sum.Warnings)
			}
		})
	}
}
