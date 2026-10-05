package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/deepnoodle-ai/wonton/cli"
	"github.com/deepnoodle-ai/wonton/color"
)

// runStatus prints the last copy run, counts by app and kind, the last
// run's warnings, copies that failed to parse, and a last line that says
// whether any of these need a look.
func runStatus(ctx *cli.Context, dir string) error {
	w := ctx.Stdout()
	dbPath := filepath.Join(dir, databaseName)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s; run agent-archive copy first", dbPath)
	}
	db, err := openDatabase(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Fprintf(w, "archive: %s\n\n", dir)

	var (
		runID, startedAt      string
		endedAt, warningsJSON sql.NullString
		seen, copied          sql.NullInt64
	)
	var warnings []string
	err = db.QueryRow(`SELECT run_id, started_at, ended_at, originals_seen, originals_copied, warnings
		FROM copy_runs ORDER BY started_at DESC, run_id DESC LIMIT 1`).
		Scan(&runID, &startedAt, &endedAt, &seen, &copied, &warningsJSON)
	switch {
	case err == sql.ErrNoRows:
		fmt.Fprintln(w, "last run: none")
	case err != nil:
		return err
	default:
		fmt.Fprintf(w, "last run: %s\n  started: %s\n", runID, startedAt)
		if endedAt.Valid {
			fmt.Fprintf(w, "  ended:   %s\n  originals seen: %d, copied: %d\n", endedAt.String, seen.Int64, copied.Int64)
		} else {
			fmt.Fprintln(w, "  ended:   never (interrupted, or still running)")
		}
		if warningsJSON.Valid {
			_ = json.Unmarshal([]byte(warningsJSON.String), &warnings)
		}
		printCount(w, "  ", "warnings", len(warnings), color.Yellow)
		for _, x := range warnings {
			fmt.Fprintf(w, "    %s\n", x)
		}
	}

	if applied, err := appliedBytes(db); err == nil {
		if info, err := os.Stat(filepath.Join(dir, copyLogName)); err == nil && info.Size() > applied {
			fmt.Fprintf(w, "\nthe copy log has %d bytes the database has not applied; the next copy applies them\n",
				info.Size()-applied)
		}
	}

	fmt.Fprintln(w, "\ncopies by app and kind:")
	rows, err := db.Query(`SELECT app, kind,
			SUM(superseded_at IS NULL),
			SUM(superseded_at IS NOT NULL),
			SUM(superseded_at IS NULL AND original_gone_at IS NOT NULL),
			SUM(parse_status = 'failed')
		FROM copies GROUP BY app, kind ORDER BY app, kind`)
	if err != nil {
		return err
	}
	var table [][]string
	for rows.Next() {
		var app, kind string
		var current, superseded, gone, failed int
		if err := rows.Scan(&app, &kind, &current, &superseded, &gone, &failed); err != nil {
			rows.Close()
			return err
		}
		table = append(table, []string{app, kind,
			strconv.Itoa(current), strconv.Itoa(superseded), strconv.Itoa(gone), strconv.Itoa(failed)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := printTable(w, []string{"app", "kind", "current", "superseded", "gone", "failed"}, table); err != nil {
		return err
	}

	rows, err = db.Query(`SELECT copy_path, COALESCE(parse_error, '') FROM copies
		WHERE parse_status = 'failed' ORDER BY copy_path`)
	if err != nil {
		return err
	}
	var failed []string
	for rows.Next() {
		var p, e string
		if err := rows.Scan(&p, &e); err != nil {
			rows.Close()
			return err
		}
		failed = append(failed, p+": "+e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	printCount(w, "", "failed files", len(failed), color.Red)
	for _, x := range failed {
		fmt.Fprintf(w, "  %s\n", x)
	}
	checks, err := printToolErrorCheck(w, db)
	if err != nil {
		return err
	}
	printVerdict(w, len(warnings)+checks, len(failed))
	return nil
}

// printVerdict prints the last line: all clear, or how many warnings and
// failed files there are.
func printVerdict(w io.Writer, warnings, failed int) {
	switch {
	case warnings == 0 && failed == 0:
		fmt.Fprintln(w, color.Green.Apply("\nall clear: no warnings and no failed files"))
	case failed == 0:
		fmt.Fprintln(w, color.Yellow.Apply(fmt.Sprintf("\nneeds a look: %s, no failed files", plural(warnings, "warning"))))
	default:
		fmt.Fprintln(w, color.Red.Apply(fmt.Sprintf("\nneeds a look: %s, %s", plural(warnings, "warning"), plural(failed, "failed file"))))
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// minCallsForErrorCheck is how many tool calls a Codex version needs before
// zero tool errors looks like a change in how Codex records exit codes.
const minCallsForErrorCheck = 200

// printToolErrorCheck warns about Codex versions with many tool calls and
// no tool errors, and returns how many warnings it printed: Codex has no
// error flag, so tool_errors comes from exit codes in output text, and a
// format change would silently zero it.
func printToolErrorCheck(w io.Writer, db *sql.DB) (int, error) {
	rows, err := db.Query(`SELECT s.app_version, SUM(s.tool_calls) FROM sessions s JOIN copies c USING (copy_id)
		WHERE s.app = ? AND c.superseded_at IS NULL AND s.tool_calls IS NOT NULL
		GROUP BY s.app_version HAVING SUM(s.tool_calls) >= ? AND SUM(s.tool_errors) = 0
		ORDER BY s.app_version`, appCodex, minCallsForErrorCheck)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var version sql.NullString
		var calls int
		if err := rows.Scan(&version, &calls); err != nil {
			return 0, err
		}
		n++
		fmt.Fprintln(w, color.Yellow.Apply(fmt.Sprintf(
			"warning: Codex %s has %d tool calls and no tool errors; check that its exit code format is still read",
			version.String, calls)))
	}
	return n, rows.Err()
}
