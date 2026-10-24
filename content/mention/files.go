package mention

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
)

// Match is an explicit mention's byte span, excluding surrounding prose.
type Match struct {
	Start, End int
	Path       string
}

// Scan finds explicit mentions without accessing the filesystem. Spans include
// line suffixes. Paths use Expand's quote and backslash grammar.
func Scan(input string) []Match {
	var matches []Match
	for _, w := range scanWords(input) {
		if !w.mention || w.path == "" {
			continue
		}
		end, path := w.pathEnd, w.path
		// Quoted paths can have a suffix outside the closing quote.
		if end < w.end && input[end] == ':' {
			tail := strings.TrimRight(input[end:w.end], trailingPunctuation)
			if lineSuffix.MatchString(tail) && lineSuffix.FindString(tail) == tail {
				path += tail
				end += len(tail)
			}
		}
		matches = append(matches, Match{w.start, end, path})
	}
	return matches
}

// File owns the bytes selected by ReadFile. CanonicalPath and LineSuffix are
// display/deduplication information, not authority for subsequent file access.
type File struct {
	Block         content.Block
	Attachment    Attachment
	CanonicalPath string
	LineSuffix    string
}

// ResolvePath resolves a selected path without reading its contents. A literal
// filename wins over a line suffix. Explicit source symlinks are permitted.
// Callers can use the canonical result to reuse an already selected snapshot.
func ResolvePath(path string, opt Options) (canonical, suffix string, err error) {
	e := expander{opt: opt.withDefaults()}
	full, err := e.resolve(path)
	if err != nil {
		return "", "", err
	}
	_, err = os.Stat(full)
	if os.IsNotExist(err) {
		if tail := lineSuffix.FindString(path); tail != "" {
			suffix = tail
			full, err = e.resolve(strings.TrimSuffix(path, tail))
			if err == nil {
				_, err = os.Stat(full)
			}
		}
	}
	if err != nil {
		return "", "", err
	}
	canonical, err = filepath.EvalSymlinks(full)
	return canonical, suffix, err
}

// ReadFile selects one file. Unlike Expand, selection failure is an error.
func ReadFile(path string, opt Options) (File, error) {
	return ReadFileContext(context.Background(), path, opt)
}

// ReadFileContext reads and sniffs one regular descriptor with bounded reads
// and cancellation checks. It never opens a special file in blocking mode.
func ReadFileContext(ctx context.Context, path string, opt Options) (File, error) {
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	canonical, suffix, err := ResolvePath(path, opt)
	if err != nil {
		return File{}, err
	}
	f, err := openSelection(canonical)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("mention: select a regular file")
	}
	o := opt.withDefaults()
	// Sniff once from this descriptor; all later reads continue from it.
	// The sniff reads its full length even under a smaller custom limit, so
	// a file signature is not cut short; the size checks below still apply.
	data, err := readSelected(ctx, f, sniffBytes)
	if err != nil {
		return File{}, err
	}
	mime := content.DetectMIMEType(data, "")
	expected := content.DetectMIMEType(nil, canonical)
	if _, known := knownExtension(canonical); known && mime != expected {
		return File{}, fmt.Errorf("mention: file signature does not match its extension")
	}
	limit := o.MaxTextBytes
	switch {
	case isImageType(mime):
		limit = o.MaxImageBytes
	case mime == "application/pdf":
		limit = o.MaxFileBytes
	default:
		mime = "text/plain"
	}
	if limit >= 0 && int64(len(data)) > limit {
		return File{}, fmt.Errorf("mention: %s file exceeds the %d-byte limit; select a smaller file", mime, limit)
	}
	remaining := int64(-1)
	if limit >= 0 {
		remaining = limit - int64(len(data)) + 1
	}
	rest, err := readSelected(ctx, f, remaining)
	if err != nil {
		return File{}, err
	}
	data = append(data, rest...)
	if limit >= 0 && int64(len(data)) > limit {
		return File{}, fmt.Errorf("mention: %s file exceeds the %d-byte limit; select a smaller file", mime, limit)
	}
	if len(data) == 0 {
		return File{}, fmt.Errorf("mention: file is empty")
	}
	if mime == "text/plain" && (!utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0) {
		return File{}, fmt.Errorf("mention: unsupported file content")
	}
	wire, err := WireSize(int64(len(data)))
	if err != nil || o.MaxTotalBytes >= 0 && wire > o.MaxTotalBytes {
		return File{}, fmt.Errorf("mention: %s file exceeds the %d-byte estimated encoded message limit; select a smaller file", mime, o.MaxTotalBytes)
	}
	source := content.Source{Data: data, MIMEType: mime}
	block := content.DocumentBlock(source, filepath.Base(canonical))
	if isImageType(mime) {
		block = content.ImageBlock(source)
	}
	return File{block, Attachment{path, mime, int64(len(data))}, canonical, suffix}, nil
}

func readSelected(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	var out []byte
	buf := make([]byte, 32<<10)
	for limit != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := buf
		if limit >= 0 {
			chunk = chunk[:min(int64(len(chunk)), limit)]
		}
		n, err := r.Read(chunk)
		out = append(out, chunk[:n]...)
		if limit >= 0 {
			limit -= int64(n)
		}
		if err == io.EOF {
			return out, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return out, ctx.Err()
}

// WireSize returns the conservative base64 request contribution, with checked
// arithmetic. It is suitable for checking aggregate selected-file limits.
func WireSize(size int64) (int64, error) {
	if size < 0 || size > (math.MaxInt64-attachmentOverhead)/4*3 {
		return 0, fmt.Errorf("mention: invalid attachment size")
	}
	return (size/3+boolInt(size%3 != 0))*4 + attachmentOverhead, nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
