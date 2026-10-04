package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// tempPrefix starts the name of a copy being written. A file with this
// prefix left in the archive is from a crashed run and is deleted.
const tempPrefix = ".agent-archive-tmp-"

// isGzipped reports whether a copy is stored gzipped (decision 3).
func isGzipped(copyPath string) bool { return strings.HasSuffix(copyPath, ".jsonl.gz") }

// writtenCopy is a temporary copy, written and synced but not yet renamed.
type writtenCopy struct {
	TempPath string
	Hash     string // SHA-256 of the uncompressed bytes
	// PrefixHash is the hash of the first prefixLen bytes, to tell an
	// appended original from a rewritten one.
	PrefixHash string
}

// writeTempCopy copies exactly size bytes of src into a temporary file
// beside dst (gzipped when dst ends in .jsonl.gz), syncs it, and gives it
// the original's modified time. Copying the size read at the start means a
// session still being written yields a clean prefix.
func writeTempCopy(src, dst string, size, prefixLen int64, modifiedAt time.Time) (wc writtenCopy, err error) {
	in, err := os.Open(src)
	if err != nil {
		return wc, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return wc, err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), tempPrefix+"*")
	if err != nil {
		return wc, err
	}
	wc.TempPath = out.Name()
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(wc.TempPath)
			wc = writtenCopy{}
		}
	}()
	full := sha256.New()
	prefix := &prefixHasher{h: sha256.New(), n: prefixLen}
	var body io.Writer = out
	var gz *gzip.Writer
	if isGzipped(dst) {
		gz = gzip.NewWriter(out)
		body = gz
	}
	n, err := io.Copy(io.MultiWriter(body, full, prefix), io.LimitReader(in, size))
	if err != nil {
		return wc, err
	}
	if n != size {
		return wc, fmt.Errorf("%s shrank while being copied (%d of %d bytes)", src, n, size)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return wc, err
		}
	}
	if err := out.Sync(); err != nil {
		return wc, err
	}
	if err := out.Close(); err != nil {
		return wc, err
	}
	if err := os.Chtimes(wc.TempPath, modifiedAt, modifiedAt); err != nil {
		return wc, err
	}
	wc.Hash = hex.EncodeToString(full.Sum(nil))
	wc.PrefixHash = hex.EncodeToString(prefix.h.Sum(nil))
	return wc, nil
}

// prefixHasher hashes only the first n bytes written to it.
type prefixHasher struct {
	h hash.Hash
	n int64
}

func (p *prefixHasher) Write(b []byte) (int, error) {
	if p.n > 0 {
		k := min(int64(len(b)), p.n)
		p.h.Write(b[:k])
		p.n -= k
	}
	return len(b), nil
}

// hashFile returns the SHA-256 of the first size bytes of a file.
func hashFile(name string, size int64) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("%s shrank while being hashed (%d of %d bytes)", name, n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openCopy opens a copy for reading its uncompressed bytes.
func openCopy(name string) (io.ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	if !isGzipped(filepath.ToSlash(name)) {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return readCloser{Reader: gz, close: f.Close}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// hashCopy returns the SHA-256 and length of a copy's uncompressed bytes.
func hashCopy(name string) (string, int64, error) {
	r, err := openCopy(name)
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashCopyPrefix returns the SHA-256 of the first n uncompressed bytes of
// a copy, or an error if the copy is shorter than n.
func hashCopyPrefix(name string, n int64) (string, error) {
	r, err := openCopy(name)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	k, err := io.Copy(h, io.LimitReader(r, n))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if k != n {
		return "", fmt.Errorf("%s has %d bytes, fewer than %d", name, k, n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// supersededMark is added to a superseded copy's name, before its
// extension: "abc.jsonl.gz" becomes "abc.superseded-20261003T153012Z.jsonl.gz".
var supersededMark = regexp.MustCompile(`\.superseded-\d{8}T\d{6}Z(-\d+)?`)

const supersededTimeLayout = "20060102T150405Z"

// supersededPath names the superseded copy of copyPath at time t: the UTC
// time is added before the extension, so names sort by time and tools still
// recognise the file type. n > 0 tells apart two in the same second.
func supersededPath(copyPath string, t time.Time, n int) string {
	dir, name := path.Split(copyPath)
	ext := path.Ext(name)
	if isGzipped(name) {
		ext = ".jsonl.gz"
	}
	if ext == name {
		ext = ""
	}
	mark := ".superseded-" + t.UTC().Format(supersededTimeLayout)
	if n > 0 {
		mark += fmt.Sprintf("-%d", n)
	}
	return dir + strings.TrimSuffix(name, ext) + mark + ext
}

// unsupersededPath reverses supersededPath: it returns the copy path a
// superseded copy had and the time in its name.
func unsupersededPath(p string) (string, time.Time, bool) {
	dir, name := path.Split(p)
	locs := supersededMark.FindAllStringIndex(name, -1)
	if locs == nil {
		return "", time.Time{}, false
	}
	loc := locs[len(locs)-1]
	stamp := name[loc[0]+len(".superseded-") : loc[0]+len(".superseded-")+len(supersededTimeLayout)]
	t, err := time.Parse(supersededTimeLayout, stamp)
	if err != nil {
		return "", time.Time{}, false
	}
	return dir + name[:loc[0]] + name[loc[1]:], t, true
}
