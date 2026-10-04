package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseSessions(t *testing.T) {
	e := newEnv(t)
	e.claude("projects/p/s-1.jsonl", claudeRootSession+"not json\n{\"no\":\"type\"}\n\n", baseTime)
	e.claude("projects/p/s-1/subagents/agent-a-7.jsonl", claudeSubagent, baseTime)
	e.codex("sessions/2026/06/22/"+codexSessionName1, codexRootSession, baseTime)
	e.codex("sessions/2026/06/22/rollout-2026-06-22T15-31-00-x.jsonl", codexSubagentSession, baseTime)
	e.claude("paste-cache/p.txt", "x", baseTime)
	e.copy()

	db := e.db()
	rows, err := db.Query(`SELECT c.copy_path, c.parse_status, c.line_types, c.parser_version,
			s.session_id, s.agent_id, s.working_dir, s.started_at, s.app_version
		FROM copies c JOIN sessions s USING (copy_id) ORDER BY c.copy_path`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ path, status, lineTypes, session, agent, dir, started, version string }
	var got []row
	for rows.Next() {
		var r row
		var pv int
		if err := rows.Scan(&r.path, &r.status, &r.lineTypes, &pv, &r.session, &r.agent, &r.dir, &r.started, &r.version); err != nil {
			t.Fatal(err)
		}
		if pv != parserVersion {
			t.Fatalf("parser_version %d", pv)
		}
		got = append(got, r)
	}
	rows.Close()
	want := []row{
		{"claude-code/projects/p/s-1.jsonl.gz", "parsed",
			`{"(bad line)":1,"(no type)":1,"assistant":1,"permission-mode":1,"user":1}`,
			"s-1", "", "/work/demo", "2026-09-01T10:00:00.000Z", "2.0.1"},
		{"claude-code/projects/p/s-1/subagents/agent-a-7.jsonl.gz", "parsed",
			`{"assistant":1,"user":1}`, "s-1", "a-7", "/work/demo", "2026-09-01T10:01:00.000Z", "2.0.1"},
		{"codex/sessions/2026/06/22/" + codexSessionName1 + ".gz", "parsed",
			`{"event_msg":1,"response_item":1,"session_meta":1}`,
			"c-1", "", "/work/other", "2026-06-22T15:30:25.000Z", "0.90.0"},
		{"codex/sessions/2026/06/22/rollout-2026-06-22T15-31-00-x.jsonl.gz", "parsed",
			`{"response_item":1,"session_meta":1}`, "c-1", "c-2", "/work/other", "2026-06-22T15:31:00.000Z", "0.90.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions:\n got %+v\nwant %+v", got, want)
	}
	var status string
	db.QueryRow(`SELECT parse_status FROM copies WHERE kind = 'paste'`).Scan(&status)
	if status != parseStatusSkipped {
		t.Fatalf("non-session file status %q", status)
	}
}

func TestParseUnreadableCopyFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl.gz")
	os.WriteFile(p, []byte("not gzip"), 0o600)
	res := parseCopy(p, appClaudeCode, kindSession)
	if res.Status != parseStatusFailed || res.Error == "" || res.Session != nil {
		t.Fatalf("result %+v", res)
	}
	res = parseCopy(filepath.Join(t.TempDir(), "missing.jsonl.gz"), appCodex, kindSession)
	if res.Status != parseStatusFailed {
		t.Fatalf("missing copy: %+v", res)
	}
}

func TestParseFailureDoesNotFailRun(t *testing.T) {
	e := newEnv(t)
	e.claude("projects/p/s.jsonl", claudeRootSession, baseTime)
	e.copy()
	// Damage the copy, then make the database parse it again.
	os.WriteFile(e.archivePath("claude-code/projects/p/s.jsonl.gz"), []byte("damaged"), 0o600)
	db := e.db()
	db.Exec(`UPDATE copies SET parser_version = NULL`)
	db.Close()
	e.touch(e.archivePath("claude-code/projects/p/s.jsonl.gz"), baseTime)
	e.copy()
	if n := e.countCopies("parse_status = 'failed' AND parse_error != ''"); n != 1 {
		t.Fatalf("failed copies %d, want 1", n)
	}
}

func TestSupersededPathRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 3, 15, 30, 12, 0, time.UTC)
	cases := map[string]string{
		"codex/sessions/2026/06/22/r.jsonl.gz": "codex/sessions/2026/06/22/r.superseded-20261003T153012Z.jsonl.gz",
		"claude-code/paste-cache/p.txt":        "claude-code/paste-cache/p.superseded-20261003T153012Z.txt",
		"claude-code/file-history/s/abc@v1":    "claude-code/file-history/s/abc@v1.superseded-20261003T153012Z",
		"codex/memories/.gitignore":            "codex/memories/.gitignore.superseded-20261003T153012Z",
	}
	for in, want := range cases {
		got := supersededPath(in, at, 0)
		if got != want {
			t.Errorf("supersededPath(%s) = %s, want %s", in, got, want)
		}
		back, gotAt, ok := unsupersededPath(got)
		if !ok || back != in || !gotAt.Equal(at) {
			t.Errorf("unsupersededPath(%s) = %s %v %v", got, back, gotAt, ok)
		}
	}
	if got := supersededPath("a/b.txt", at, 2); got != "a/b.superseded-20261003T153012Z-2.txt" {
		t.Errorf("numbered: %s", got)
	}
	if back, _, ok := unsupersededPath("a/b.superseded-20261003T153012Z-2.txt"); !ok || back != "a/b.txt" {
		t.Errorf("numbered round trip: %s", back)
	}
}
