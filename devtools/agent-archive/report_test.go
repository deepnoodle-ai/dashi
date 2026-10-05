package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newYork is a fixed zone for period tests, so they never depend on the
// machine's.
var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

func date(s string) time.Time {
	t, err := time.ParseInLocation(time.DateOnly, s, newYork)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPeriods(t *testing.T) {
	for _, c := range []struct {
		day, per, start, label string
	}{
		{"2026-10-05", perWeek, "2026-10-05", "2026-10-05"}, // a Monday
		{"2026-10-04", perWeek, "2026-09-28", "2026-09-28"}, // a Sunday
		{"2026-10-01", perWeek, "2026-09-28", "2026-09-28"},
		{"2026-10-04", perMonth, "2026-10-01", "2026-10"},
		{"2026-10-04", perDay, "2026-10-04", "2026-10-04"},
	} {
		got := periodStart(date(c.day), c.per)
		if !got.Equal(date(c.start)) || periodLabel(got, c.per) != c.label {
			t.Errorf("%s per %s: %s %q, want %s %q", c.day, c.per, got.Format(time.DateOnly),
				periodLabel(got, c.per), c.start, c.label)
		}
	}
}

func TestReportOptionsDefaults(t *testing.T) {
	now := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC) // Monday morning in New York
	for _, c := range []struct {
		per, since string
		periods    int
	}{
		{perWeek, "2026-08-17", 8}, // the design note's example
		{perDay, "2026-09-28", 8},
		{perMonth, "2026-03-01", 8},
	} {
		o, err := newReportOptions(byModel, c.per, "", "", now, newYork)
		if err != nil {
			t.Fatal(err)
		}
		if o.Since.Format(time.DateOnly) != c.since || o.Until.Format(time.DateOnly) != "2026-10-05" {
			t.Errorf("per %s: %s to %s, want %s to 2026-10-05", c.per, o.Since.Format(time.DateOnly),
				o.Until.Format(time.DateOnly), c.since)
		}
		if n := len(o.periods()); n != c.periods {
			t.Errorf("per %s: %d periods, want %d", c.per, n, c.periods)
		}
	}
	// Local time: 02:00 UTC on the 6th is still the 5th in New York.
	o, _ := newReportOptions(byModel, perWeek, "", "", time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC), newYork)
	if o.Until.Format(time.DateOnly) != "2026-10-05" {
		t.Errorf("until %s, want the local day 2026-10-05", o.Until.Format(time.DateOnly))
	}
	if _, err := newReportOptions(byModel, perWeek, "2026-10-06", "2026-10-05", now, newYork); err == nil {
		t.Error("since after until: no error")
	}
	if _, err := newReportOptions(byModel, perWeek, "10/05/2026", "", now, newYork); err == nil {
		t.Error("bad date: no error")
	}
}

// reportDB is a database with hand-written rows, for report tests.
type reportDB struct {
	t    *testing.T
	db   *sql.DB
	dir  string
	next int64
}

func newReportDB(t *testing.T) *reportDB {
	dir := t.TempDir()
	db, err := openDatabase(filepath.Join(dir, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &reportDB{t: t, db: db, dir: dir}
}

func (r *reportDB) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.db.Exec(q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

// session adds a session file and returns its copy_id. cost < 0 means
// none.
func (r *reportDB) session(app, sessionID, agentID, dir, started, remote string, cost float64) int64 {
	r.next++
	r.exec(`INSERT INTO copies (copy_id, app, copy_path, original_path, kind, original_bytes, modified_at, hash, copied_at,
		parse_status, parser_version) VALUES (?, ?, ?, '', 'session', 0, '', '', '', 'parsed', ?)`,
		r.next, app, "c/"+sessionID+agentID, parserVersion)
	var c any
	if cost >= 0 {
		c = cost
	}
	r.exec(`INSERT INTO sessions (copy_id, app, session_id, agent_id, working_dir, started_at, git_remote_url, app_cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.next, app, sessionID, agentID, dir, started, remote, c)
	return r.next
}

// usage adds a usage row; calledAt "" stores NULL.
func (r *reportDB) usage(copyID int64, line int, model string, t tokenCounts, speed, calledAt string) {
	var at any
	if calledAt != "" {
		at = calledAt
	}
	r.exec(`INSERT INTO usage (copy_id, line, model_call_id, model, input_tokens, cache_read_input_tokens,
		cache_creation_input_tokens, cache_creation_1h_input_tokens, output_tokens, reasoning_tokens,
		web_search_requests, speed, called_at) VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		copyID, line, model, t.Input, t.CacheRead, t.CacheWrite, t.CacheWrite1h, t.Output, t.Reasoning, t.WebSearch, speed, at)
}

func (r *reportDB) appUsage(copyID int64, model string, t tokenCounts) {
	r.exec(`INSERT INTO app_usage (copy_id, model, input_tokens, cache_read_input_tokens, cache_creation_input_tokens,
		output_tokens, reasoning_tokens, web_search_requests, cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		copyID, model, t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.WebSearch)
}

func (r *reportDB) report(o reportOptions) (string, string) {
	r.t.Helper()
	var out, warn bytes.Buffer
	if err := runReport(r.dir, o, &out, &warn); err != nil {
		r.t.Fatal(err)
	}
	return out.String(), warn.String()
}

// Prices in this test come from the catalog: claude-opus-5-5 is $4 input,
// $20 output, $0.20 cache read, $5 cache write, $8 one-hour cache write.
func TestReport(t *testing.T) {
	r := newReportDB(t)
	const home = "/home/me"
	mtok := tokenCounts{Output: 1_000_000} // $20 on claude-opus-5-5

	// A Claude Code session in a worktree with a subagent, a call with no
	// called_at (its session started on the 1st), and calls not in
	// session files. Its app cost is in the range.
	root := r.session(appClaudeCode, "s1", "", home+"/git/app/.claude/worktrees/x", "2026-10-01T14:00:00Z", "", 60)
	sub := r.session(appClaudeCode, "s1", "a1", home+"/git/app/.claude/worktrees/x", "2026-10-01T15:00:00Z", "", -1)
	r.usage(root, 1, "claude-opus-5-5", mtok, "", "2026-10-02T03:00:00Z") // the 1st in New York
	r.usage(sub, 1, "claude-opus-5-5", mtok, "", "")
	r.usage(root, 2, "claude-opus-5-5", tokenCounts{CacheWrite: 1_000_000, CacheWrite1h: 1_000_000}, "", "2026-10-05T16:00:00Z")
	// app_usage: 2M output, as archived, and 3M cache writes, of which
	// 1M are archived: 2M writes not in session files, $10 to $16, on the
	// session's last call day.
	r.appUsage(root, "claude-opus-5-5[1m]", tokenCounts{Output: 2_000_000, CacheWrite: 1_000_000})
	r.appUsage(root, "claude-opus-5-5", tokenCounts{CacheWrite: 2_000_000})

	// Sessions whose app cost is lower, higher, and not compared (a fast
	// call).
	low := r.session(appClaudeCode, "s2", "", home+"/git/app", "2026-09-29T14:00:00Z", "", 10)
	r.usage(low, 1, "claude-opus-5-5", mtok, "", "2026-09-29T14:00:00Z")
	high := r.session(appClaudeCode, "s3", "", home+"/git/app", "2026-09-29T14:00:00Z", "", 30)
	r.usage(high, 1, "claude-opus-5-5", mtok, "", "2026-09-29T14:00:00Z")
	fast := r.session(appClaudeCode, "s4", "", home+"/git/app", "2026-09-29T14:00:00Z", "", 20)
	r.usage(fast, 1, "claude-opus-5-5", mtok, speedFast, "2026-09-29T14:00:00Z")

	// Codex: a priced call (a high estimate) in a worktree of a project,
	// an unpriced model, and a call before the report's days.
	cx := r.session(appCodex, "c1", "", home+"/.codex/worktrees/ab12/tool", "2026-10-03T12:00:00Z", "", -1)
	r.session(appCodex, "c0", "", home+"/git/tool", "2026-01-01T12:00:00Z", "", -1)
	r.usage(cx, 1, "gpt-5.6", tokenCounts{Input: 250_000}, "", "2026-10-03T12:00:00Z") // $1 to $1.25
	r.usage(cx, 2, "codex-auto-review", tokenCounts{Input: 7}, "", "2026-10-03T12:00:00Z")
	r.usage(cx, 3, "gpt-5.6", tokenCounts{Input: 1_000_000}, "", "2026-07-01T12:00:00Z")

	o := reportOptions{By: byModel, Per: perWeek, Since: date("2026-09-28"), Until: date("2026-10-05"),
		Location: newYork, Home: home}
	out, warn := r.report(o)
	if warn != "" {
		t.Errorf("warnings: %s", warn)
	}
	for _, want := range []string{
		"Model                                   2026-09-28  2026-10-05     Total\n",
		// s1's two $20 output calls and s2 to s4's, then s1's 1M one-hour
		// writes ($8, on the 5th); the fast call makes a lower bound.
		"claude-opus-5-5                           $100.00+       $8.00  $108.00+\n",
		"claude-opus-5-5 (not in session files)                  $16.00    $16.00\n",
		"gpt-5.6                                      $1.25                 $1.25\n",
		"codex-auto-review                                ?                     ?\n",
		"Total                                     $101.25+      $24.00  $125.25+\n",
		"--json gives the low end ($119.00 in all)",
		"codex-auto-review  codex  not in catalog      1      7           0            0       0\n",
		"Claude Code's own session totals against Dashi's: 1 agree, 1 lower, 1 higher, 1 not compared\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}

	o.By = byProject
	out, _ = r.report(o)
	for _, want := range []string{"\n~/git/app  ", "\n~/git/tool  "} {
		if !strings.Contains(out, want) {
			t.Errorf("by project lacks %q:\n%s", want, out)
		}
	}

	o.By, o.JSON = byModel, true
	out, warn = r.report(o)
	var rows []spendJSON
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var j spendJSON
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		rows = append(rows, j)
	}
	found := 0
	for _, j := range rows {
		switch {
		case j.Name == "gpt-5.6":
			found++
			if j.Low == nil || *j.Low != amt("1") || *j.High != amt("1.25") || j.Amount != amt("1.25") ||
				j.PriceUpdatedAt == "" {
				t.Errorf("gpt-5.6: %+v", j)
			}
		case j.Name == "codex-auto-review":
			found++
			if !j.LowerBound || j.UnpricedCalls != 1 {
				t.Errorf("codex-auto-review: %+v", j)
			}
		case strings.HasSuffix(j.Name, notInSessionFiles):
			found++
			if j.Period != "2026-10-05" || *j.Low != amt("10") || *j.High != amt("16") {
				t.Errorf("not in session files: %+v", j)
			}
		}
	}
	if found != 3 || !strings.Contains(warn, "1 agree") {
		t.Errorf("json:\n%s\n%s", out, warn)
	}

	o.By, o.JSON = bySession, false
	out, _ = r.report(o)
	lines := strings.Split(out, "\n")
	// s2 $20 vs $10; s3 $20 vs $30; s1 $48 + $10 to $16 = $58 to $64 vs $60.
	for i, want := range []string{
		"Session  App          Project     Started        Dashi cost  App cost  Difference",
		"s2       claude_code  ~/git/app   2026-09-29         $20.00    $10.00     -$10.00",
		"s3       claude_code  ~/git/app   2026-09-29         $20.00    $30.00      $10.00",
		"s1       claude_code  ~/git/app   2026-10-01  $58.00–$64.00    $60.00       $0.00",
		"s4       claude_code  ~/git/app   2026-09-29        $20.00+    $20.00",
		// The whole session, July's long-input call too, at the high end.
		"c1       codex        ~/git/tool  2026-10-03        $11.25+",
	} {
		if lines[2+i] != want {
			t.Errorf("line %d: %q, want %q\n%s", 2+i, lines[2+i], want, out)
		}
	}
}

func TestReportWarnsOfOlderParser(t *testing.T) {
	r := newReportDB(t)
	r.session(appCodex, "c1", "", "/x", "2026-10-03T12:00:00Z", "", -1)
	r.exec(`UPDATE copies SET parser_version = 4`)
	_, warn := r.report(reportOptions{By: byModel, Per: perWeek, Since: date("2026-09-28"),
		Until: date("2026-10-05"), Location: newYork})
	if !strings.Contains(warn, "1 session files were parsed by an older agent-archive") {
		t.Errorf("warn: %q", warn)
	}
}

func TestReportNeedsCalledAt(t *testing.T) {
	r := newReportDB(t)
	r.exec(`ALTER TABLE usage DROP COLUMN called_at`)
	r.db.Close()
	err := runReport(r.dir, reportOptions{By: byModel, Per: perWeek, Location: newYork}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run agent-archive parse") {
		t.Errorf("err %v", err)
	}
}
