package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv is a temporary home with Claude Code and Codex directories and
// an archive. All fixtures are hand-written.
type testEnv struct {
	t    *testing.T
	opts options
}

var baseTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	e := &testEnv{t: t, opts: options{
		ArchiveDir: filepath.Join(root, "agent-archive"),
		ClaudeDir:  filepath.Join(root, ".claude"),
		CodexDir:   filepath.Join(root, ".codex"),
	}}
	for _, d := range []string{e.opts.ClaudeDir, e.opts.CodexDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// write writes an original below dir with the given modified time.
func (e *testEnv) write(dir, rel, content string, mtime time.Time) string {
	e.t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.touch(p, mtime)
	return p
}

func (e *testEnv) touch(p string, mtime time.Time) {
	e.t.Helper()
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) claude(rel, content string, mtime time.Time) string {
	return e.write(e.opts.ClaudeDir, rel, content, mtime)
}

func (e *testEnv) codex(rel, content string, mtime time.Time) string {
	return e.write(e.opts.CodexDir, rel, content, mtime)
}

func (e *testEnv) copy() copySummary {
	e.t.Helper()
	sum, err := runCopy(e.opts)
	if err != nil {
		e.t.Fatalf("copy: %v", err)
	}
	return sum
}

// events returns every event in the copy log.
func (e *testEnv) events() []logEvent {
	e.t.Helper()
	evs, _, err := readLog(filepath.Join(e.opts.ArchiveDir, copyLogName), 0)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]logEvent, len(evs))
	for i, ev := range evs {
		out[i] = ev.logEvent
	}
	return out
}

// eventsOf returns the events of one kind, from the given index on.
func eventsOf(evs []logEvent, from int, kind string) []logEvent {
	var out []logEvent
	for _, ev := range evs[from:] {
		if ev.Event == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (e *testEnv) db() *sql.DB {
	e.t.Helper()
	db, err := openDatabase(filepath.Join(e.opts.ArchiveDir, databaseName))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return db
}

// dump renders every table, in a stable order, for comparing databases.
func (e *testEnv) dump() string {
	e.t.Helper()
	db := e.db()
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT * FROM copies ORDER BY copy_id`,
		`SELECT * FROM sessions ORDER BY copy_id`,
		`SELECT * FROM copy_runs ORDER BY run_id`,
		`SELECT * FROM log_position`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			e.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		fmt.Fprintf(&b, "%s\n", strings.Join(cols, "|"))
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				e.t.Fatal(err)
			}
			for i, v := range vals {
				if i > 0 {
					b.WriteString("|")
				}
				fmt.Fprintf(&b, "%v", v)
			}
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

// copyRowAt returns the copy whose copy_path is p.
func (e *testEnv) copyRowAt(p string) (row copyRow, superseded sql.NullString) {
	e.t.Helper()
	db := e.db()
	defer db.Close()
	err := db.QueryRow(`SELECT copy_id, app, copy_path, original_path, original_bytes, modified_at, hash, original_gone_at, superseded_at
		FROM copies WHERE copy_path = ?`, p).Scan(&row.CopyID, &row.App, &row.CopyPath, &row.OriginalPath,
		&row.OriginalBytes, &row.ModifiedAt, &row.Hash, &row.OriginalGoneAt, &superseded)
	if err != nil {
		e.t.Fatalf("copy %s: %v", p, err)
	}
	return row, superseded
}

func (e *testEnv) countCopies(where string) int {
	e.t.Helper()
	db := e.db()
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM copies WHERE ` + where).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// readCopy returns a copy's uncompressed bytes.
func (e *testEnv) readCopy(copyPath string) string {
	e.t.Helper()
	r, err := openCopy(filepath.Join(e.opts.ArchiveDir, filepath.FromSlash(copyPath)))
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *testEnv) archivePath(copyPath string) string {
	return filepath.Join(e.opts.ArchiveDir, filepath.FromSlash(copyPath))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func isGzipFile(t *testing.T, p string) bool {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.HasPrefix(b, []byte{0x1f, 0x8b})
}

// Hand-written session lines.
const (
	claudeRootSession = `{"type":"permission-mode","permissionMode":"default","sessionId":"s-1"}
{"type":"user","sessionId":"s-1","cwd":"/work/demo","version":"2.0.1","timestamp":"2026-09-01T10:00:00.000Z","uuid":"u1","message":{"role":"user","content":"hello"}}
{"type":"assistant","sessionId":"s-1","cwd":"/work/demo","version":"2.0.1","timestamp":"2026-09-01T10:00:01.000Z","uuid":"u2","message":{"role":"assistant","content":[]}}
`
	claudeSubagent = `{"type":"user","sessionId":"s-1","agentId":"a-7","isSidechain":true,"cwd":"/work/demo","version":"2.0.1","timestamp":"2026-09-01T10:01:00.000Z"}
{"type":"assistant","sessionId":"s-1","agentId":"a-7","isSidechain":true,"cwd":"/work/demo","version":"2.0.1","timestamp":"2026-09-01T10:01:02.000Z"}
`
	codexRootSession = `{"timestamp":"2026-06-22T15:30:25.000Z","type":"session_meta","payload":{"id":"c-1","session_id":"c-1","cwd":"/work/other","cli_version":"0.90.0","timestamp":"2026-06-22T15:30:25.000Z","source":"cli"}}
{"timestamp":"2026-06-22T15:30:26.000Z","type":"response_item","payload":{"type":"message"}}
{"timestamp":"2026-06-22T15:30:27.000Z","type":"event_msg","payload":{"type":"token_count"}}
`
	codexSubagentSession = `{"timestamp":"2026-06-22T15:31:00.000Z","type":"session_meta","payload":{"id":"c-2","session_id":"c-1","parent_thread_id":"c-1","cwd":"/work/other","cli_version":"0.90.0","timestamp":"2026-06-22T15:31:00.000Z","source":{"subagent":{"thread_spawn":{}}}}}
{"timestamp":"2026-06-22T15:31:01.000Z","type":"response_item","payload":{}}
`
)
