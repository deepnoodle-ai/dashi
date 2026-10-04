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

// deleteTempFiles deletes temporary files left by an interrupted run, the
// last recovery rule (decision 6); openArchive applies the others. A crash
// between a rename and its log append needs no recovery: the next run
// copies the original again.
func (a *archive) deleteTempFiles() error {
	for _, folder := range appFolders {
		root := filepath.Join(a.Dir, folder)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == root {
					return nil
				}
				return err
			}
			if d.Type().IsRegular() && strings.HasPrefix(d.Name(), tempPrefix) {
				a.warn("deleted a temporary file left by an interrupted run: %s", a.rel(p))
				return os.Remove(p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
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
