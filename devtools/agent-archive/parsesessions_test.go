package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture reads a hand-written session file from a parser package's
// testdata.
func fixture(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Codex fixture file names, dated as the archive layout needs.
const (
	codexRootName   = "rollout-2026-07-10T06-00-00-r-root.jsonl"
	codexForkName   = "rollout-2026-07-10T06-00-02-r-fork.jsonl"
	codexNestedName = "rollout-2026-07-10T06-00-03-r-nested.jsonl"
)

// writeSessions writes a Claude Code session with a fork and a Codex
// session with a fork and a nested subagent, plus Codex's titles.
func writeSessions(t *testing.T, e *testEnv) {
	e.claude("projects/-work-demo/s-fork.jsonl", fixture(t, "claudecode/testdata/fork/root.jsonl"), baseTime)
	e.claude("projects/-work-demo/s-fork/subagents/agent-af.jsonl", fixture(t, "claudecode/testdata/fork/agent-af.jsonl"), baseTime)
	e.claude("projects/-work-demo/s-fork/subagents/agent-af.meta.json", fixture(t, "claudecode/testdata/fork/agent-af.meta.json"), baseTime)
	e.codex("sessions/2026/07/10/"+codexRootName, fixture(t, "codex/testdata/fork/root.jsonl"), baseTime)
	e.codex("sessions/2026/07/10/"+codexForkName, fixture(t, "codex/testdata/fork/helper.jsonl"), baseTime)
	e.codex("sessions/2026/07/10/"+codexNestedName, fixture(t, "codex/testdata/fork/nested.jsonl"), baseTime)
	e.codex("session_index.jsonl", `{"id":"r-root","thread_name":"Old title"}`+"\n"+
		`{"id":"r-root","thread_name":"Spread the work"}`+"\n", baseTime)
}

type sessionRow struct {
	Path, Session, Agent, Kind, Parent, Call, Title, Source string
	Prompts, Calls, Usage                                   int
}

func (e *testEnv) sessionRows() []sessionRow {
	e.t.Helper()
	db := e.db()
	defer db.Close()
	rows, err := db.Query(`SELECT c.copy_path, COALESCE(s.session_id, ''), s.agent_id, COALESCE(s.agent_kind, ''),
			COALESCE(s.parent_agent_id, ''), COALESCE(s.parent_tool_call_id, ''), COALESCE(s.title, ''),
			COALESCE(s.usage_source, ''), COALESCE(s.user_prompts, 0), COALESCE(s.tool_calls, 0),
			(SELECT COUNT(*) FROM usage u WHERE u.copy_id = c.copy_id)
		FROM copies c JOIN sessions s USING (copy_id) ORDER BY c.copy_path`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.Path, &r.Session, &r.Agent, &r.Kind, &r.Parent, &r.Call, &r.Title, &r.Source,
			&r.Prompts, &r.Calls, &r.Usage); err != nil {
			e.t.Fatal(err)
		}
		r.Path = filepath.Base(r.Path)
		out = append(out, r)
	}
	return out
}

func TestParseFillsSessionsAndUsage(t *testing.T) {
	e := newEnv(t)
	writeSessions(t, e)
	e.copy()
	got := e.sessionRows()
	want := []sessionRow{
		{"s-fork.jsonl.gz", "s-fork", "", "root", "", "", "", "message_usage", 1, 1, 2},
		{"agent-af.jsonl.gz", "s-fork", "af", "fork", "", "toolu_f", "Placeholder fork", "message_usage", 1, 1, 1},
		{codexRootName + ".gz", "r-root", "", "root", "", "", "Spread the work", "token_usage_record", 1, 1, 2},
		{codexForkName + ".gz", "r-root", "r-fork", "fork", "", "call_sp", "", "token_usage_record", 0, 1, 2},
		// The nested subagent's file names itself as its session; linking
		// walks up to the root.
		{codexNestedName + ".gz", "r-root", "r-nested", "subagent", "r-fork", "call_nest", "", "token_usage_record", 1, 0, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions:\n got %+v\nwant %+v", got, want)
	}
	if n := e.countCopies(`parser_version = 2`); n != 7 {
		t.Fatalf("%d copies at the current parser version, want 7", n)
	}

	// A second run parses nothing.
	a, err := openArchive(e.opts.ArchiveDir)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := parsePending(a.DB, a.Dir)
	a.Close()
	if err != nil || sum.Copies != 0 || sum.Sessions != 0 {
		t.Fatalf("second parse: %+v %v", sum, err)
	}
}

func TestStaleFactsAreDeleted(t *testing.T) {
	e := newEnv(t)
	root := fixture(t, "claudecode/testdata/fork/root.jsonl")
	p := e.claude("projects/-work-demo/s-fork.jsonl", root, baseTime)
	e.copy()
	if got := e.sessionRows(); len(got) != 1 || got[0].Usage != 2 {
		t.Fatalf("first copy: %+v", got)
	}

	// Rewritten: the old copy keeps only phase 1 facts and no usage.
	first, _, _ := strings.Cut(root, "\n")
	e.write(filepath.Dir(p), filepath.Base(p), first+"\n", baseTime.Add(1))
	e.copy()
	got := e.sessionRows()
	if len(got) != 2 {
		t.Fatalf("rows: %+v", got)
	}
	old, cur := got[1], got[0]
	if !strings.Contains(old.Path, ".superseded-") {
		old, cur = got[0], got[1]
	}
	if old.Kind != "" || old.Usage != 0 || old.Session != "s-fork" {
		t.Fatalf("superseded copy: %+v", old)
	}
	if cur.Kind != "root" || cur.Usage != 0 || cur.Prompts != 1 {
		t.Fatalf("current copy: %+v", cur)
	}

	// Appended: the copy is rewritten in place and parsed again.
	e.write(filepath.Dir(p), filepath.Base(p), root, baseTime.Add(2))
	e.copy()
	for _, r := range e.sessionRows() {
		if !strings.Contains(r.Path, ".superseded-") && r.Usage != 2 {
			t.Fatalf("appended copy: %+v", r)
		}
	}
}

func TestStatusWarnsWhenCodexToolErrorsVanish(t *testing.T) {
	e := newEnv(t)
	writeSessions(t, e)
	e.copy()
	db := e.db()
	if _, err := db.Exec(`UPDATE sessions SET tool_calls = 500, tool_errors = 0 WHERE app = 'codex'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r := runCLI(t, "status", "--archive", e.opts.ArchiveDir)
	if r.ExitCode != 0 || !r.Contains("warning: Codex 0.144.0 has 1000 tool calls and no tool errors") {
		t.Fatalf("status exit %d:\n%s", r.ExitCode, r.Stdout)
	}
}

func TestParseAllAndConvertCommands(t *testing.T) {
	e := newEnv(t)
	writeSessions(t, e)
	e.copy()

	r := runCLI(t, "parse", "--all", "--archive", e.opts.ArchiveDir)
	if r.ExitCode != 0 {
		t.Fatalf("parse exit %d: %v", r.ExitCode, r.Err)
	}
	for _, want := range []string{"sessions parsed: 2 (5 session files)", "usage rows:      8"} {
		if !r.Contains(want) {
			t.Errorf("parse output lacks %q:\n%s", want, r.Stdout)
		}
	}

	r = runCLI(t, "convert", "--archive", e.opts.ArchiveDir, "--session", "r-root", "--agent", "r-fork")
	if r.ExitCode != 0 {
		t.Fatalf("convert exit %d: %v %s", r.ExitCode, r.Err, r.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(lines) != 6 {
		t.Fatalf("convert printed %d lines:\n%s", len(lines), r.Stdout)
	}
	var first convertedLine
	dec := json.NewDecoder(bytes.NewReader([]byte(lines[0])))
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if !first.Ref.Inherited || first.Message.Text() != "Spread the work." {
		t.Fatalf("first line: %+v", first)
	}
	if !strings.Contains(r.Stderr, "6 messages") || !strings.Contains(r.Stderr, "dropped:encrypted_content") {
		t.Fatalf("convert report:\n%s", r.Stderr)
	}

	if r := runCLI(t, "convert", "--archive", e.opts.ArchiveDir, "--session", "nope"); r.ExitCode != 1 {
		t.Fatalf("unknown session: exit %d", r.ExitCode)
	}
	if r := runCLI(t, "convert", "--archive", e.opts.ArchiveDir); r.ExitCode != 1 {
		t.Fatalf("missing --session: exit %d", r.ExitCode)
	}
}
