package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// databaseName is the SQLite database built from the copy log and the copies.
const databaseName = "archive.sqlite"

const schema = `
CREATE TABLE IF NOT EXISTS copies (
	copy_id          INTEGER PRIMARY KEY,
	app              TEXT NOT NULL,
	copy_path        TEXT NOT NULL UNIQUE,
	original_path    TEXT NOT NULL,
	kind             TEXT NOT NULL,
	original_bytes   INTEGER NOT NULL,
	modified_at      TEXT NOT NULL,
	hash             TEXT NOT NULL,
	copied_at        TEXT NOT NULL,
	superseded_at    TEXT,
	original_gone_at TEXT,
	parse_status     TEXT,
	parse_error      TEXT,
	parser_version   INTEGER,
	line_types       TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
	copy_id     INTEGER PRIMARY KEY REFERENCES copies (copy_id),
	app         TEXT NOT NULL,
	session_id  TEXT,
	agent_id    TEXT NOT NULL,
	working_dir TEXT,
	started_at  TEXT,
	app_version TEXT
);
CREATE INDEX IF NOT EXISTS sessions_session_id ON sessions (session_id);
-- One row per model call; see design/agent-archive-parse.md.
CREATE TABLE IF NOT EXISTS usage (
	copy_id                        INTEGER NOT NULL REFERENCES copies (copy_id),
	line                           INTEGER NOT NULL,
	model_call_id                  TEXT NOT NULL,
	model                          TEXT NOT NULL,
	input_tokens                   INTEGER NOT NULL,
	cache_read_input_tokens        INTEGER NOT NULL,
	cache_creation_input_tokens    INTEGER NOT NULL,
	cache_creation_1h_input_tokens INTEGER NOT NULL,
	output_tokens                  INTEGER NOT NULL,
	reasoning_tokens               INTEGER NOT NULL,
	web_search_requests            INTEGER NOT NULL,
	speed                          TEXT NOT NULL,
	PRIMARY KEY (copy_id, line)
);
CREATE TABLE IF NOT EXISTS copy_runs (
	run_id           TEXT PRIMARY KEY,
	started_at       TEXT NOT NULL,
	ended_at         TEXT,
	originals_seen   INTEGER,
	originals_copied INTEGER,
	warnings         TEXT
);
-- How many bytes of copy-log.jsonl this database has applied.
CREATE TABLE IF NOT EXISTS log_position (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	applied_bytes INTEGER NOT NULL
);
INSERT OR IGNORE INTO log_position (id, applied_bytes) VALUES (1, 0);
`

// openDatabase opens or creates the database at path, owner-only.
func openDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := addSessionColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite gives its -wal and -shm files the database file's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// sessionColumns are the sessions columns phase 2 parsing fills. A
// database made before phase 2 gets them added, and the parser version
// bump then fills them.
var sessionColumns = []struct{ name, typ string }{
	{"agent_kind", "TEXT"},
	{"parent_agent_id", "TEXT"},
	{"parent_tool_call_id", "TEXT"},
	{"forked_from_session_id", "TEXT"},
	{"entrypoint", "TEXT"},
	{"git_branch", "TEXT"},
	{"git_remote_url", "TEXT"},
	{"title", "TEXT"},
	{"ended_at", "TEXT"},
	{"user_prompts", "INTEGER"},
	{"tool_calls", "INTEGER"},
	{"tool_errors", "INTEGER"},
	{"compactions", "INTEGER"},
	{"app_cost_usd", "REAL"},
	{"usage_source", "TEXT"},
	{"skipped_lines", "TEXT"},
}

func addSessionColumns(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range sessionColumns {
		if !have[c.name] {
			if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + c.name + ` ` + c.typ); err != nil {
				return err
			}
		}
	}
	return nil
}

func appliedBytes(db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRow(`SELECT applied_bytes FROM log_position WHERE id = 1`).Scan(&n)
	return n, err
}

// applyEvent records one log event and the new log position in a single
// transaction, so the database never holds half an event.
func applyEvent(db *sql.DB, ev loggedEvent) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyEventTx(tx, ev.logEvent); err != nil {
		return fmt.Errorf("apply %s event: %w", ev.Event, err)
	}
	if _, err := tx.Exec(`UPDATE log_position SET applied_bytes = ? WHERE id = 1`, ev.End); err != nil {
		return err
	}
	return tx.Commit()
}

func applyEventTx(tx *sql.Tx, ev logEvent) error {
	at := formatTime(ev.Time)
	switch ev.Event {
	case eventRunStarted:
		_, err := tx.Exec(`INSERT OR IGNORE INTO copy_runs (run_id, started_at) VALUES (?, ?)`, ev.RunID, at)
		return err
	case eventRunEnded:
		warnings := []string{}
		if ev.Warnings != nil {
			warnings = *ev.Warnings
		}
		w, err := json.Marshal(warnings)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE copy_runs SET ended_at = ?, originals_seen = ?, originals_copied = ?, warnings = ?
			WHERE run_id = ?`, at, intOrNil(ev.OriginalsSeen), intOrNil(ev.OriginalsCopied), string(w), ev.RunID)
		return err
	case eventCopyWritten:
		if ev.OriginalBytes == nil || ev.ModifiedAt == nil {
			return fmt.Errorf("copy_written for %s lacks original_bytes or modified_at", ev.CopyPath)
		}
		var id int64
		err := tx.QueryRow(`SELECT copy_id FROM copies WHERE copy_path = ?`, ev.CopyPath).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`INSERT INTO copies (app, copy_path, original_path, kind, original_bytes, modified_at, hash, copied_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.App, ev.CopyPath, ev.OriginalPath, ev.Kind, *ev.OriginalBytes, formatTime(*ev.ModifiedAt), ev.Hash, at)
			return err
		case err != nil:
			return err
		}
		// The copy was rewritten in place (its original grew); what was
		// parsed from it is stale.
		if err := deleteFacts(tx, id); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE copies SET app = ?, original_path = ?, kind = ?, original_bytes = ?, modified_at = ?, hash = ?,
			copied_at = ?, superseded_at = NULL, original_gone_at = NULL,
			parse_status = NULL, parse_error = NULL, parser_version = NULL, line_types = NULL
			WHERE copy_id = ?`,
			ev.App, ev.OriginalPath, ev.Kind, *ev.OriginalBytes, formatTime(*ev.ModifiedAt), ev.Hash, at, id)
		return err
	case eventCopySuperseded:
		// A superseded copy keeps only phase 1 facts.
		if err := clearPhase2Facts(tx, ev.CopyPath); err != nil {
			return err
		}
		return execOne(tx, `UPDATE copies SET copy_path = ?, superseded_at = ? WHERE copy_path = ?`,
			ev.SupersededPath, at, ev.CopyPath)
	case eventModifiedAtUpdated:
		if ev.ModifiedAt == nil {
			return fmt.Errorf("modified_at_updated for %s lacks modified_at", ev.CopyPath)
		}
		return execOne(tx, `UPDATE copies SET modified_at = ? WHERE copy_path = ?`, formatTime(*ev.ModifiedAt), ev.CopyPath)
	case eventOriginalGone:
		return execOne(tx, `UPDATE copies SET original_gone_at = ? WHERE copy_path = ?`, at, ev.CopyPath)
	}
	return fmt.Errorf("unknown event %q", ev.Event)
}

// deleteFacts deletes everything parsed from a copy.
func deleteFacts(tx *sql.Tx, copyID int64) error {
	if _, err := tx.Exec(`DELETE FROM usage WHERE copy_id = ?`, copyID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM sessions WHERE copy_id = ?`, copyID)
	return err
}

// clearPhase2Facts deletes a copy's usage rows and empties its phase 2
// sessions columns.
func clearPhase2Facts(tx *sql.Tx, copyPath string) error {
	var id int64
	err := tx.QueryRow(`SELECT copy_id FROM copies WHERE copy_path = ?`, copyPath).Scan(&id)
	if err == sql.ErrNoRows {
		return nil // execOne reports it
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM usage WHERE copy_id = ?`, id); err != nil {
		return err
	}
	set := make([]string, len(sessionColumns))
	for i, c := range sessionColumns {
		set[i] = c.name + " = NULL"
	}
	_, err = tx.Exec(`UPDATE sessions SET `+strings.Join(set, ", ")+` WHERE copy_id = ?`, id)
	return err
}

// execOne runs an update that must touch exactly one copy.
func execOne(tx *sql.Tx, query string, args ...any) error {
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("no copy at %v", args[len(args)-1])
	}
	return nil
}

func intOrNil(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// timeLayout is how every time is stored: UTC, RFC 3339 with all nine
// fraction digits, so stored times sort as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// copyRow is what the copy rule needs to know about a current copy.
type copyRow struct {
	CopyID         int64
	App            string
	CopyPath       string
	OriginalPath   string
	OriginalBytes  int64
	ModifiedAt     string
	Hash           string
	OriginalGoneAt sql.NullString
}

// currentCopies returns every copy not superseded, by copy path.
func currentCopies(db *sql.DB) (map[string]copyRow, error) {
	rows, err := db.Query(`SELECT copy_id, app, copy_path, original_path, original_bytes, modified_at, hash, original_gone_at
		FROM copies WHERE superseded_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]copyRow{}
	for rows.Next() {
		var r copyRow
		if err := rows.Scan(&r.CopyID, &r.App, &r.CopyPath, &r.OriginalPath, &r.OriginalBytes, &r.ModifiedAt, &r.Hash, &r.OriginalGoneAt); err != nil {
			return nil, err
		}
		out[r.CopyPath] = r
	}
	return out, rows.Err()
}
