package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/claudecode"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/codex"
	"github.com/deepnoodle-ai/dashi/devtools/agent-archive/sessionfile"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/wonton/color"
)

// runParse parses copies that need it; all parses every copy again.
func runParse(dir string, all bool) (parseSummary, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return parseSummary{}, fmt.Errorf("no archive at %s", dir)
	}
	a, err := openArchive(dir)
	if err != nil {
		return parseSummary{}, err
	}
	defer a.Close()
	if all {
		if _, err := a.DB.Exec(`UPDATE copies SET parser_version = NULL`); err != nil {
			return parseSummary{}, err
		}
	}
	sum, err := parsePending(a.DB, a.Dir)
	sum.Warnings = append(a.Warnings, sum.Warnings...)
	return sum, err
}

func printParseSummary(w io.Writer, s parseSummary) {
	fmt.Fprintf(w, "copies read:     %d\n", s.Copies)
	fmt.Fprintf(w, "sessions parsed: %d (%d session files)\n", s.Sessions, s.SessionCopies)
	fmt.Fprintf(w, "usage rows:      %d\n", s.UsageRows)
	for _, x := range s.Warnings {
		fmt.Fprintln(w, color.Yellow.Apply("warning: "+x))
	}
}

// convertedLine is one line of convert's output.
type convertedLine struct {
	Ref     sessionfile.LineRef `json:"ref"`
	Message llm.Message         `json:"message"`
}

// runConvert writes one agent's transcript to out, one JSON line per
// message, and its reports to report.
func runConvert(dir, session, agent string, out, report io.Writer) error {
	dbPath := filepath.Join(dir, databaseName)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s; run agent-archive copy first", dbPath)
	}
	db, err := openDatabase(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	copies, app, err := sessionCopies(db, dir, session)
	if err != nil {
		return err
	}
	convert := claudecode.Convert
	if app == appCodex {
		convert = codex.Convert
	}
	t, err := convert(copies, agent)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for i, m := range t.Messages {
		if err := enc.Encode(convertedLine{Ref: t.LineRefs[i], Message: m}); err != nil {
			return err
		}
	}
	fmt.Fprintf(report, "%d messages\n", len(t.Messages))
	printReport(report, "skipped lines", t.SkippedLines)
	printReport(report, "changed blocks", t.ChangedBlocks)
	return nil
}

func printReport(w io.Writer, label string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "%s:\n", label)
	for _, k := range keys {
		fmt.Fprintf(w, "  %-32s %d\n", k, counts[k])
	}
}

// sessionCopies returns the current, parsed copies of a session.
func sessionCopies(db *sql.DB, dir, session string) ([]sessionfile.Copy, string, error) {
	rows, err := db.Query(`SELECT c.copy_id, c.app, c.copy_path, s.agent_id, COALESCE(s.started_at, '')
		FROM copies c JOIN sessions s USING (copy_id)
		WHERE s.session_id = ? AND c.superseded_at IS NULL AND c.parse_status = ? ORDER BY c.copy_id`,
		session, parseStatusParsed)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	a := archive{Dir: dir}
	var copies []sessionfile.Copy
	app := ""
	for rows.Next() {
		var c sessionfile.Copy
		var copyApp string
		if err := rows.Scan(&c.ID, &copyApp, &c.Name, &c.AgentID, &c.StartedAt); err != nil {
			return nil, "", err
		}
		if app != "" && copyApp != app {
			return nil, "", fmt.Errorf("session %s has copies from both apps", session)
		}
		app = copyApp
		name := a.abs(c.Name)
		c.Open = func() (io.ReadCloser, error) { return openCopy(name) }
		copies = append(copies, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(copies) == 0 {
		return nil, "", fmt.Errorf("no parsed session %s", session)
	}
	return copies, app, nil
}
