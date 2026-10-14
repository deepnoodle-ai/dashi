package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// copyOutcome says what the copy rule did with one original.
type copyOutcome int

const (
	copyOutcomeSkipped copyOutcome = iota
	copyOutcomeModifiedAtUpdated
	copyOutcomeWritten    // first copy
	copyOutcomeRewritten  // the original grew; the copy was rewritten
	copyOutcomeSuperseded // the old copy was kept under a new name
)

// copyOriginal applies the copy rule (decision 4) to one original, given
// its newest copy (nil if none):
//   - same size and modified time: skip;
//   - same size and same hash: record the new modified time only;
//   - larger, and its first old-size bytes hash to the stored hash: it was
//     appended to, so rewrite the copy;
//   - anything else: keep the old copy under a superseded name and write a
//     new one.
//
// If the newest copy is missing from the archive (someone deleted it), the
// original is copied again as if it had no copy, with a warning, so an
// original is never left without one.
//
// An original that was marked gone and is listed again is recorded as
// returned first, whatever the rule then does.
//
// An error here is about this original only; the run goes on. Errors from
// the copy log or database are returned wrapped in fatalError.
func (a *archive) copyOriginal(o original, prev *copyRow) (copyOutcome, error) {
	if prev != nil {
		if _, err := os.Lstat(a.abs(prev.CopyPath)); errors.Is(err, fs.ErrNotExist) {
			a.warn("the newest copy of %s was missing, so it was copied again: %s", o.Path, prev.CopyPath)
			prev = nil
		}
	}
	if prev != nil && prev.OriginalGoneAt.Valid {
		// The original was marked gone but is back. A new copy_written
		// would also clear the mark, but an unchanged original writes none.
		if err := a.record(logEvent{Event: eventOriginalReturned, CopyPath: o.CopyPath}); err != nil {
			return 0, fatal(err)
		}
	}
	modifiedAt := formatTime(o.ModifiedAt)
	if prev != nil && prev.OriginalBytes == o.Size {
		if prev.ModifiedAt == modifiedAt {
			return copyOutcomeSkipped, nil
		}
		h, err := hashFile(o.Path, o.Size)
		if err != nil {
			return 0, err
		}
		if h == prev.Hash {
			// Keep the copy's own modified time equal to the original's.
			_ = os.Chtimes(a.abs(o.CopyPath), o.ModifiedAt, o.ModifiedAt)
			t := o.ModifiedAt
			return copyOutcomeModifiedAtUpdated, fatal(a.record(logEvent{
				Event: eventModifiedAtUpdated, CopyPath: o.CopyPath, ModifiedAt: &t,
			}))
		}
	}
	var prefixLen int64
	if prev != nil && o.Size > prev.OriginalBytes {
		prefixLen = prev.OriginalBytes
	}
	dst := a.abs(o.CopyPath)
	wc, err := writeTempCopy(o.Path, dst, o.Size, prefixLen, o.ModifiedAt)
	if err != nil {
		return 0, err
	}
	outcome := copyOutcomeWritten
	switch {
	case prev == nil:
	case o.Size > prev.OriginalBytes && wc.PrefixHash == prev.Hash:
		outcome = copyOutcomeRewritten
	default:
		outcome = copyOutcomeSuperseded
		now := time.Now()
		old, err := a.supersede(o.CopyPath, now)
		if err != nil {
			os.Remove(wc.TempPath)
			return 0, err
		}
		if err := os.Rename(wc.TempPath, dst); err != nil {
			return 0, err
		}
		syncDir(dst)
		if err := a.record(logEvent{
			Event: eventCopySuperseded, Time: now, CopyPath: o.CopyPath, SupersededPath: old,
		}); err != nil {
			return 0, fatal(err)
		}
		return outcome, fatal(a.recordWritten(o, wc.Hash))
	}
	if err := os.Rename(wc.TempPath, dst); err != nil {
		os.Remove(wc.TempPath)
		return 0, err
	}
	syncDir(dst)
	return outcome, fatal(a.recordWritten(o, wc.Hash))
}

func (a *archive) recordWritten(o original, hash string) error {
	size, t := o.Size, o.ModifiedAt
	return a.record(logEvent{
		Event:         eventCopyWritten,
		CopyPath:      o.CopyPath,
		OriginalPath:  o.Path,
		App:           o.App,
		Kind:          o.Kind,
		OriginalBytes: &size,
		ModifiedAt:    &t,
		Hash:          hash,
	})
}

// supersede renames the copy at copyPath to its superseded name and
// returns that name. The file is kept and stays readable.
func (a *archive) supersede(copyPath string, t time.Time) (string, error) {
	for n := 0; ; n++ {
		p := supersededPath(copyPath, t, n)
		if _, err := os.Lstat(a.abs(p)); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(a.abs(copyPath), a.abs(p)); err != nil {
				return "", err
			}
			return p, nil
		}
	}
}

// syncDir makes a rename in the directory holding name durable. Some
// systems can't sync a directory; a failure only weakens durability, and
// recovery handles a lost rename, so it is ignored.
func syncDir(name string) {
	d, err := os.Open(filepath.Dir(name))
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}

// fatalError marks an error from the copy log or database, which stops a
// run, as opposed to an error about one original, which becomes a warning.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

func fatal(err error) error {
	if err == nil {
		return nil
	}
	return fatalError{err}
}
