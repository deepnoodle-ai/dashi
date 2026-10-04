package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// recoverCopies applies the last two recovery rules (decision 6), after
// openArchive has applied the first two:
//   - a leftover temporary file is deleted;
//   - a copy file the log doesn't know (a crash between rename and append)
//     is hashed and logged with recovered: true, and a renamed old copy is
//     logged as superseded.
//
// originals maps copy paths to this run's originals, to fill in a
// recovered copy's original path.
func (a *archive) recoverCopies(originals map[string]original) error {
	known, err := allCopyPaths(a.DB)
	if err != nil {
		return err
	}
	var unknown []string
	for _, folder := range appFolders {
		root := filepath.Join(a.Dir, folder)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == root {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if strings.HasPrefix(d.Name(), tempPrefix) {
				a.warn("deleted a temporary file left by an interrupted run: %s", a.rel(p))
				return os.Remove(p)
			}
			if rel := a.rel(p); !known[rel] {
				unknown = append(unknown, rel)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Superseded copies first: renaming the old copy comes before the new
	// copy takes its place, and the new copy is then unknown too.
	sort.Slice(unknown, func(i, j int) bool {
		_, _, si := unsupersededPath(unknown[i])
		_, _, sj := unsupersededPath(unknown[j])
		if si != sj {
			return si
		}
		return unknown[i] < unknown[j]
	})
	current, err := currentCopies(a.DB)
	if err != nil {
		return err
	}
	for i := 0; i < len(unknown); i++ {
		p := unknown[i]
		if base, t, ok := unsupersededPath(p); ok {
			if _, isCurrent := current[base]; isCurrent {
				if err := a.record(logEvent{
					Event: eventCopySuperseded, Time: t, CopyPath: base, SupersededPath: p, Recovered: true,
				}); err != nil {
					return err
				}
				a.warn("logged a superseded copy an interrupted run did not log: %s", p)
				delete(current, base)
				// A file now at base is the new copy, which the log
				// doesn't know either.
				if _, err := os.Lstat(a.abs(base)); err == nil {
					unknown = append(unknown, base)
				}
				continue
			}
		}
		if err := a.recoverUnknownCopy(p, originals[p]); err != nil {
			return err
		}
	}
	return nil
}

// recoverUnknownCopy logs a copy the log doesn't know. Its modified time is
// the original's, because copies are given their original's modified time
// before the rename.
func (a *archive) recoverUnknownCopy(copyPath string, o original) error {
	app, kind, _ := classifyCopyPath(copyPath)
	full := a.abs(copyPath)
	hash, size, err := hashCopy(full)
	if err != nil {
		a.warn("could not read a copy the log does not know, left as is: %s: %v", copyPath, err)
		return nil
	}
	info, err := os.Stat(full)
	if err != nil {
		return err
	}
	o = original{
		Path:       o.Path, // empty if no original maps here this run
		App:        app,
		Kind:       kind,
		CopyPath:   copyPath,
		Size:       size,
		ModifiedAt: info.ModTime().UTC(),
	}
	a.warn("logged a copy an interrupted run did not log: %s", copyPath)
	return a.recordWritten(o, hash, true)
}

// rel returns the archive-relative, slash-separated form of a path.
func (a *archive) rel(p string) string {
	r, err := filepath.Rel(a.Dir, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// markGone records original_gone for each current copy of app whose
// original was not seen this run, and returns how many it marked.
func (a *archive) markGone(app string, seen map[string]bool) (int, error) {
	current, err := currentCopies(a.DB)
	if err != nil {
		return 0, err
	}
	var gone []string
	for p, c := range current {
		if c.App == app && !seen[p] && !c.OriginalGoneAt.Valid {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	now := time.Now()
	for _, p := range gone {
		if err := a.record(logEvent{Event: eventOriginalGone, Time: now, CopyPath: p}); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}
