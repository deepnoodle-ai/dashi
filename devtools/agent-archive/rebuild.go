package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// rebuildSummary is what a rebuild did.
type rebuildSummary struct {
	Events   int
	Warnings []string
}

// runRebuild recreates the database from the copy log plus the copies: the
// log gives every recorded fact, and the current parser reads the copies
// again. It writes a new database beside the old one and swaps it in, so
// a failed rebuild leaves the old database as it was. It does not change
// the copy log or the copies; recovery is the copy command's job.
func runRebuild(dir string) (rebuildSummary, error) {
	var sum rebuildSummary
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return sum, fmt.Errorf("no archive at %s", dir)
	}
	lock, err := lockArchive(dir)
	if err != nil {
		return sum, err
	}
	defer lock.Close()

	logPath := filepath.Join(dir, copyLogName)
	events, validEnd, err := readLog(logPath, 0)
	if err != nil {
		return sum, err
	}
	if info, err := os.Stat(logPath); err == nil && info.Size() > validEnd {
		sum.Warnings = append(sum.Warnings, fmt.Sprintf(
			"ignored a torn last line (%d bytes) in %s; the next copy run drops it", info.Size()-validEnd, copyLogName))
	}

	final := filepath.Join(dir, databaseName)
	tmp := final + ".rebuild"
	removeDatabase(tmp)
	db, err := openDatabase(tmp)
	if err != nil {
		return sum, err
	}
	for _, ev := range events {
		if err := applyEvent(db, ev); err != nil {
			db.Close()
			return sum, err
		}
	}
	if err := parsePending(db, dir); err != nil {
		db.Close()
		return sum, err
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		return sum, err
	}
	if err := db.Close(); err != nil {
		return sum, err
	}
	// An old -wal file must not be applied to the new database.
	os.Remove(final + "-wal")
	os.Remove(final + "-shm")
	if err := os.Rename(tmp, final); err != nil {
		return sum, err
	}
	removeDatabase(tmp)
	sum.Events = len(events)
	return sum, nil
}

func removeDatabase(name string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(name + suffix)
	}
}

func printRebuildSummary(w io.Writer, dir string, s rebuildSummary) {
	fmt.Fprintf(w, "rebuilt %s from %d copy log events\n", filepath.Join(dir, databaseName), s.Events)
	for _, x := range s.Warnings {
		fmt.Fprintf(w, "warning: %s\n", x)
	}
}
