package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// runStatus prints the last copy run, counts by app and kind, the last
// run's warnings, and copies that failed to parse.
func runStatus(dir string, w io.Writer) error {
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
		var warnings []string
		if warningsJSON.Valid {
			_ = json.Unmarshal([]byte(warningsJSON.String), &warnings)
		}
		fmt.Fprintf(w, "  warnings: %d\n", len(warnings))
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
	fmt.Fprintf(w, "  %-12s %-16s %8s %11s %9s %7s\n", "app", "kind", "current", "superseded", "gone", "failed")
	rows, err := db.Query(`SELECT app, kind,
			SUM(superseded_at IS NULL),
			SUM(superseded_at IS NOT NULL),
			SUM(superseded_at IS NULL AND original_gone_at IS NOT NULL),
			SUM(parse_status = 'failed')
		FROM copies GROUP BY app, kind ORDER BY app, kind`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var app, kind string
		var current, superseded, gone, failed int
		if err := rows.Scan(&app, &kind, &current, &superseded, &gone, &failed); err != nil {
			rows.Close()
			return err
		}
		fmt.Fprintf(w, "  %-12s %-16s %8d %11d %9d %7d\n", app, kind, current, superseded, gone, failed)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = db.Query(`SELECT copy_path, COALESCE(parse_error, '') FROM copies
		WHERE parse_status = 'failed' ORDER BY copy_path`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var failed []string
	for rows.Next() {
		var p, e string
		if err := rows.Scan(&p, &e); err != nil {
			return err
		}
		failed = append(failed, p+": "+e)
	}
	fmt.Fprintf(w, "\nfailed files: %d\n", len(failed))
	for _, x := range failed {
		fmt.Fprintf(w, "  %s\n", x)
	}
	return rows.Err()
}
