package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// verifyRow is what verify needs to know about one copy.
type verifyRow struct {
	CopyPath      string
	OriginalBytes int64
	Hash          string
	Superseded    bool
}

// verifyProblem is one file that failed verify, and why.
type verifyProblem struct {
	Path   string
	Reason string
}

// verifyResult is what verify found.
type verifyResult struct {
	Current, Superseded int
	Missing             []string        // copies missing from disk
	Mismatched          []verifyProblem // copies whose bytes differ from the database
	Unreadable          []verifyProblem
	Snapshot            *snapshotResult // set when -snapshot was given
}

// snapshotResult is what verify -snapshot found (decision 7). Paths are
// relative to the snapshot directory.
type snapshotResult struct {
	Dir   string
	Files int // files checked
	Equal int // a copy equals the file
	Grew  int // a longer copy begins with the file: the original grew since
	// Failed files have no copy that equals or begins with them. They must
	// be kept.
	Failed []verifyProblem
	// NotCopied are files of kinds copy does not copy, so verify can't
	// check them. They are not failures, but they have no copy either.
	NotCopied []string
}

func (r verifyResult) problems() int {
	n := len(r.Missing) + len(r.Mismatched) + len(r.Unreadable)
	if r.Snapshot != nil {
		n += len(r.Snapshot.Failed)
	}
	return n
}

// snapshotFolders maps a snapshot's top folders to apps: a snapshot holds
// claude/ and codex/, mirroring ~/.claude and ~/.codex.
var snapshotFolders = map[string]string{
	"claude": appClaudeCode,
	"codex":  appCodex,
}

// runVerify re-hashes every copy, current and superseded, against the
// database, and with snapshotDir also checks a snapshot. It changes no
// copy and nothing in the snapshot; opening the archive may apply its
// first two recovery rules and tighten permissions.
func runVerify(archiveDir, snapshotDir string) (verifyResult, error) {
	var res verifyResult
	if _, err := os.Stat(filepath.Join(archiveDir, databaseName)); err != nil {
		return res, fmt.Errorf("no database in %s; run agent-archive copy first", archiveDir)
	}
	a, err := openArchive(archiveDir)
	if err != nil {
		return res, err
	}
	defer a.Close()
	rows, err := verifyRows(a)
	if err != nil {
		return res, err
	}
	for _, r := range rows {
		if r.Superseded {
			res.Superseded++
		} else {
			res.Current++
		}
		hash, n, err := hashCopy(a.abs(r.CopyPath))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.Missing = append(res.Missing, r.CopyPath)
		case err != nil:
			res.Unreadable = append(res.Unreadable, verifyProblem{r.CopyPath, err.Error()})
		case hash != r.Hash || n != r.OriginalBytes:
			res.Mismatched = append(res.Mismatched, verifyProblem{r.CopyPath, fmt.Sprintf(
				"%d bytes with hash %.12s, but the database has %d bytes with hash %.12s",
				n, hash, r.OriginalBytes, r.Hash)})
		}
	}
	if snapshotDir != "" {
		s, err := verifySnapshot(a, snapshotDir, rows)
		if err != nil {
			return res, err
		}
		res.Snapshot = &s
	}
	return res, nil
}

func verifyRows(a *archive) ([]verifyRow, error) {
	rows, err := a.DB.Query(`SELECT copy_path, original_bytes, hash, superseded_at IS NOT NULL
		FROM copies ORDER BY copy_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verifyRow
	for rows.Next() {
		var r verifyRow
		if err := rows.Scan(&r.CopyPath, &r.OriginalBytes, &r.Hash, &r.Superseded); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// verifySnapshot checks each snapshot file against the copies of the
// original it was taken from. It maps the file to a copy path the way copy
// maps an original, then accepts it when a copy at that path, current or
// superseded, equals it or begins with it.
func verifySnapshot(a *archive, dir string, rows []verifyRow) (snapshotResult, error) {
	res := snapshotResult{Dir: dir}
	// Every copy of one original, by its current copy path: the current
	// copy first, then superseded ones, newest first.
	byCopyPath := map[string][]verifyRow{}
	for _, r := range rows {
		p := r.CopyPath
		if r.Superseded {
			if base, _, ok := unsupersededPath(p); ok {
				p = base
			}
		}
		byCopyPath[p] = append(byCopyPath[p], r)
	}
	for _, cs := range byCopyPath {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Superseded != cs[j].Superseded {
				return !cs[i].Superseded
			}
			return cs[i].CopyPath > cs[j].CopyPath // superseded names sort by time
		})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	found := false
	for _, entry := range entries {
		app, ok := snapshotFolders[entry.Name()]
		if !ok || !entry.IsDir() {
			res.NotCopied = append(res.NotCopied, entry.Name())
			continue
		}
		found = true
		appDir := filepath.Join(dir, entry.Name())
		err := filepath.WalkDir(appDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(appDir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			snapRel := entry.Name() + "/" + rel
			if !d.Type().IsRegular() || !isCopied(app, rel) {
				res.NotCopied = append(res.NotCopied, snapRel)
				return nil
			}
			res.Files++
			copyPath := copyPathFor(app, rel)
			equal, reason := checkSnapshotFile(a, p, copyPath, byCopyPath[copyPath])
			switch {
			case reason != "":
				res.Failed = append(res.Failed, verifyProblem{snapRel, reason})
			case equal:
				res.Equal++
			default:
				res.Grew++
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	if !found {
		return res, fmt.Errorf("no claude/ or codex/ folder in snapshot %s", dir)
	}
	return res, nil
}

// checkSnapshotFile looks for a copy that equals the snapshot file at name
// (equal true) or begins with it (equal false). reason says why it failed,
// and is empty when a copy was found.
func checkSnapshotFile(a *archive, name, copyPath string, copies []verifyRow) (equal bool, reason string) {
	info, err := os.Stat(name)
	if err != nil {
		return false, err.Error()
	}
	size := info.Size()
	want, err := hashFile(name, size)
	if err != nil {
		return false, err.Error()
	}
	if len(copies) == 0 {
		return false, "no copy at " + copyPath
	}
	for _, c := range copies {
		if c.OriginalBytes < size {
			continue
		}
		// A copy that can't be read is reported by the first pass.
		got, err := hashCopyPrefix(a.abs(c.CopyPath), size)
		if err == nil && got == want {
			return c.OriginalBytes == size, ""
		}
	}
	return false, fmt.Sprintf("none of the %d copies at %s equals or begins with it", len(copies), copyPath)
}

func printVerifyResult(w io.Writer, archiveDir string, r verifyResult) {
	fmt.Fprintf(w, "archive: %s\n", archiveDir)
	fmt.Fprintf(w, "copies checked: %d (%d current, %d superseded)\n", r.Current+r.Superseded, r.Current, r.Superseded)
	fmt.Fprintf(w, "  missing from disk: %d\n", len(r.Missing))
	for _, p := range r.Missing {
		fmt.Fprintf(w, "    %s\n", p)
	}
	printProblems(w, "  bytes differ from the database", r.Mismatched)
	printProblems(w, "  unreadable", r.Unreadable)
	if s := r.Snapshot; s != nil {
		fmt.Fprintf(w, "\nsnapshot: %s\n", s.Dir)
		fmt.Fprintf(w, "files checked: %d\n", s.Files)
		fmt.Fprintf(w, "  accepted: %d (%d equal a copy, %d are the start of a longer copy)\n",
			s.Equal+s.Grew, s.Equal, s.Grew)
		printProblems(w, "  failed, keep these", s.Failed)
		fmt.Fprintf(w, "  not checked, because copy does not copy them: %d\n", len(s.NotCopied))
		for _, p := range s.NotCopied {
			fmt.Fprintf(w, "    %s\n", p)
		}
	}
	if n := r.problems(); n == 0 {
		fmt.Fprintln(w, "\nverify passed")
	} else {
		fmt.Fprintf(w, "\nverify failed: %d problems\n", n)
	}
}

func printProblems(w io.Writer, title string, ps []verifyProblem) {
	fmt.Fprintf(w, "%s: %d\n", title, len(ps))
	for _, p := range ps {
		fmt.Fprintf(w, "    %s: %s\n", p.Path, p.Reason)
	}
}
