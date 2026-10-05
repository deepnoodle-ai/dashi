package main

import (
	"strconv"
	"testing"
)

// TestCopyAddsAndFillsCalledAt checks a database made before
// usage.called_at: opening it adds the column, and the next copy parses
// every session again and fills it from each row's line.
func TestCopyAddsAndFillsCalledAt(t *testing.T) {
	e := newEnv(t)
	writeSessions(t, e)
	e.copy()

	// Make the database look as an older version left it.
	db := e.db()
	for _, q := range []string{
		`ALTER TABLE usage DROP COLUMN called_at`,
		`UPDATE copies SET parser_version = 4 WHERE parser_version IS NOT NULL`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	e.copy()
	if n := e.countCopies(`parser_version = ` + strconv.Itoa(parserVersion)); n != 7 {
		t.Fatalf("%d copies at the current parser version, want 7", n)
	}
	db = e.db()
	defer db.Close()
	var rows, filled int
	var first string
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(called_at), MIN(called_at) FROM usage`).
		Scan(&rows, &filled, &first); err != nil {
		t.Fatal(err)
	}
	if rows != 8 || filled != 8 {
		t.Fatalf("%d usage rows, %d with called_at; want 8 and 8", rows, filled)
	}
	// Each value is a line's timestamp as written, like sessions.started_at.
	var started string
	if err := db.QueryRow(`SELECT MIN(started_at) FROM sessions`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if len(first) != len(started) || first < started {
		t.Fatalf("earliest called_at %q, earliest started_at %q", first, started)
	}
}
