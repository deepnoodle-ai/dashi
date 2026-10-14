package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

const (
	defaultReadLimit = 2000
	// readMaxLine bounds one shown line. The tool cuts a longer line and
	// says so, so a minified file cannot fill memory or the result.
	readMaxLine = 2000
	// readMaxBytes bounds the text of one result. The tool stops showing
	// lines at the bound and tells the model the offset to read on from.
	readMaxBytes = 256 << 10
)

type readArgs struct {
	Path   string `json:"path" description:"File to read. Relative to the workspace, or absolute inside it."`
	Offset int    `json:"offset,omitempty" description:"First line to read, counting from 1. Default 1."`
	Limit  int    `json:"limit,omitempty" description:"Maximum number of lines to read. Default 2000."`
}

func newReadFile(s *scope) (tool.Tool, error) {
	return closedWorld(tool.Func(
		ToolReadFile,
		"Read a text file. Each line starts with its line number and a tab. "+
			"Use offset and limit to read part of a long file. "+
			"The tool refuses binary files.",
		func(ctx context.Context, _ tool.Call, in readArgs) (tool.Outcome, error) {
			return readFile(ctx, s, in)
		},
		tool.ReadOnly(),
	))
}

func readFile(ctx context.Context, s *scope, in readArgs) (tool.Outcome, error) {
	path, err := s.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail(fmt.Sprintf("file %s does not exist. Use glob or list_directory to find it.", in.Path))
	case err != nil:
		return fail(fmt.Sprintf("cannot read %s: %v", in.Path, err))
	case info.IsDir():
		return fail(fmt.Sprintf("%s is a directory. Use list_directory to see its files.", in.Path))
	case !info.Mode().IsRegular():
		// Opening a named pipe or a device can block forever.
		return fail(fmt.Sprintf("%s is not a regular file. The read_file tool reads regular files only.", in.Path))
	}
	if in.Offset < 0 || in.Limit < 0 {
		return fail("offset and limit must not be negative.")
	}
	offset, limit := max(in.Offset, 1), in.Limit
	if limit == 0 {
		limit = defaultReadLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(fmt.Sprintf("cannot read %s: %v", in.Path, err))
	}
	defer f.Close()
	r := bufio.NewReaderSize(ctxReader{ctx, f}, 64<<10)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return fail(fmt.Sprintf("%s is a binary file. The read_file tool reads text only.", in.Path))
	}
	var out strings.Builder
	total, shown, full := 0, 0, false
	// line keeps the start of the current line, at most readMaxLine bytes
	// plus room for \r\n. size counts all of its bytes.
	var line []byte
	size := 0
	for {
		chunk, err := r.ReadSlice('\n')
		size += len(chunk)
		if room := readMaxLine + 2 - len(line); room > 0 {
			line = append(line, chunk[:min(room, len(chunk))]...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if size > 0 {
			total++
			if total >= offset && shown < limit && !full {
				text := formatReadLine(total, line, size)
				if out.Len()+len(text) > readMaxBytes {
					full = true
				} else {
					out.WriteString(text)
					shown++
				}
			}
			line, size = line[:0], 0
		}
		if err == io.EOF {
			break
		}
		if ctx.Err() != nil {
			return fail(fmt.Sprintf("reading %s was canceled.", in.Path))
		}
		if err != nil {
			return fail(fmt.Sprintf("cannot read %s: %v", in.Path, err))
		}
	}
	switch {
	case total == 0:
		return tool.Succeeded("The file is empty."), nil
	case offset > total:
		return fail(fmt.Sprintf("offset %d is past the end of %s, which has %d lines.", offset, in.Path, total))
	}
	if last := offset + shown - 1; last < total {
		fmt.Fprintf(&out, "[Showed lines %d-%d of %d. Set offset to %d to read more.]\n", offset, last, total, last+1)
	}
	return tool.Succeeded(out.String()), nil
}

// formatReadLine numbers one line. line holds the start of the line and size
// counts all of its bytes, with the line end. A line longer than readMaxLine
// is cut, and a note gives its full length.
func formatReadLine(n int, line []byte, size int) string {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) <= readMaxLine && size <= readMaxLine+2 {
		return fmt.Sprintf("%6d\t%s\n", n, line)
	}
	text := strings.ToValidUTF8(string(line[:min(len(line), readMaxLine)]), "")
	return fmt.Sprintf("%6d\t%s... [line cut at %d of %d bytes]\n", n, text, readMaxLine, size)
}

type listArgs struct {
	Path string `json:"path,omitempty" description:"Directory to list. Default is the workspace root."`
}

func newListDirectory(s *scope) (tool.Tool, error) {
	return closedWorld(tool.Func(
		ToolListDirectory,
		"List the entries of one directory. A name that ends with / is a directory.",
		func(_ context.Context, _ tool.Call, in listArgs) (tool.Outcome, error) {
			path, err := s.resolve(in.Path)
			if err != nil {
				return fail(err.Error())
			}
			entries, err := os.ReadDir(path)
			switch {
			case errors.Is(err, os.ErrNotExist):
				return fail(fmt.Sprintf("directory %s does not exist.", in.Path))
			case err != nil:
				return fail(fmt.Sprintf("cannot list %s: %v", in.Path, err))
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			slices.Sort(names)
			if len(names) == 0 {
				return tool.Succeeded("The directory is empty."), nil
			}
			return tool.Succeeded(strings.Join(names, "\n")), nil
		},
		tool.ReadOnly(),
	))
}
