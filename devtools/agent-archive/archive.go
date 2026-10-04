package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockName is the file a command holds an exclusive lock on, so two runs
// never write the archive at once.
const lockName = "archive.lock"

// archive is an open archive directory: its lock, copy log, and database.
type archive struct {
	Dir   string
	RunID string
	DB    *sql.DB

	lock *os.File
	log  *os.File
	// logEnd is the copy log's size, which is also the position just past
	// the last event.
	logEnd int64
	// Warnings collects what a run reports in run_ended.
	Warnings []string
}

// openArchive creates the archive directory if needed, takes the lock,
// tightens loose permissions, drops a torn last log line, and applies log
// events the database has not seen. Those are the first two recovery
// rules; deleteTempFiles handles the last once the run has started.
func openArchive(dir string) (*archive, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &archive{Dir: dir, RunID: newRunID(time.Now())}
	ok := false
	defer func() {
		if !ok {
			a.Close()
		}
	}()
	var err error
	if a.lock, err = lockArchive(dir); err != nil {
		return nil, err
	}
	if n, err := fixPermissions(dir); err != nil {
		return nil, err
	} else if n > 0 {
		a.warn("fixed permissions looser than 0700/0600 on %d entries in %s", n, dir)
	}
	logPath := filepath.Join(dir, copyLogName)
	if a.DB, err = openDatabase(filepath.Join(dir, databaseName)); err != nil {
		return nil, err
	}
	from, err := appliedBytes(a.DB)
	if err != nil {
		return nil, err
	}
	events, validEnd, err := readLog(logPath, from)
	if err != nil {
		return nil, err
	}
	if a.log, err = os.OpenFile(logPath, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return nil, err
	}
	info, err := a.log.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > validEnd {
		a.warn("dropped a torn last line (%d bytes) from %s", info.Size()-validEnd, copyLogName)
		if err := a.log.Truncate(validEnd); err != nil {
			return nil, err
		}
		if err := a.log.Sync(); err != nil {
			return nil, err
		}
	}
	a.logEnd = validEnd
	for _, ev := range events {
		if err := applyEvent(a.DB, ev); err != nil {
			return nil, err
		}
	}
	ok = true
	return a, nil
}

// Close releases the database, log, and lock.
func (a *archive) Close() error {
	var errs []error
	if a.DB != nil {
		errs = append(errs, a.DB.Close())
	}
	if a.log != nil {
		errs = append(errs, a.log.Close())
	}
	if a.lock != nil {
		errs = append(errs, a.lock.Close()) // closing releases the flock
	}
	return errors.Join(errs...)
}

func (a *archive) warn(format string, args ...any) {
	a.Warnings = append(a.Warnings, fmt.Sprintf(format, args...))
}

// record appends one event to the copy log and syncs it, then applies it
// to the database together with the new log position (decision 6).
func (a *archive) record(ev logEvent) error {
	if ev.RunID == "" {
		ev.RunID = a.RunID
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	line, err := encodeEvent(ev)
	if err != nil {
		return err
	}
	if _, err := a.log.WriteAt(line, a.logEnd); err != nil {
		return fmt.Errorf("append to copy log: %w", err)
	}
	if err := a.log.Sync(); err != nil {
		return fmt.Errorf("sync copy log: %w", err)
	}
	a.logEnd += int64(len(line))
	return applyEvent(a.DB, loggedEvent{logEvent: ev, End: a.logEnd})
}

// abs returns the absolute path of an archive-relative copy path.
func (a *archive) abs(copyPath string) string {
	return filepath.Join(a.Dir, filepath.FromSlash(copyPath))
}

func newRunID(t time.Time) string {
	var b [3]byte
	rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func lockArchive(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another agent-archive command is using %s", dir)
	}
	return f, nil
}

// fixPermissions makes every directory in the archive 0700 and every file
// 0600 where they are looser, and returns how many it changed.
func fixPermissions(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		if perm&0o077 == 0 {
			return nil
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		n++
		return os.Chmod(p, want)
	})
	return n, err
}
