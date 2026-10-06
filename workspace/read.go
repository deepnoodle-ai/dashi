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

const defaultReadLimit = 2000

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
		func(_ context.Context, _ tool.Call, in readArgs) (tool.Outcome, error) {
			return readFile(s, in)
		},
		tool.ReadOnly(),
	))
}

func readFile(s *scope, in readArgs) (tool.Outcome, error) {
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
	r := bufio.NewReader(f)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return fail(fmt.Sprintf("%s is a binary file. The read_file tool reads text only.", in.Path))
	}
	var out strings.Builder
	total, shown := 0, 0
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			total++
			if total >= offset && shown < limit {
				fmt.Fprintf(&out, "%6d\t%s\n", total, strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
				shown++
			}
		}
		if err == io.EOF {
			break
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
