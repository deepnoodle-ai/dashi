package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deepnoodle-ai/dashi/tool"
)

type writeArgs struct {
	Path    string `json:"path" description:"File to write. Relative to the workspace, or absolute inside it."`
	Content string `json:"content" description:"The complete new content of the file."`
}

func newWriteFile(s *scope) (tool.Tool, error) {
	t, err := tool.Func(
		ToolWriteFile,
		"Write a whole file. Create the file when it does not exist, and "+
			"replace it when it does. Create missing parent directories. "+
			"To change part of a file, use edit_file.",
		func(_ context.Context, _ tool.Call, in writeArgs) (tool.Outcome, error) {
			path, err := s.resolve(in.Path)
			if err != nil {
				return fail(err.Error())
			}
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				return fail(fmt.Sprintf("%s is a directory. Give a file path.", in.Path))
			} else if err == nil && !info.Mode().IsRegular() {
				// Opening a named pipe can block forever.
				return fail(fmt.Sprintf("%s is not a regular file.", in.Path))
			}
			verb := "Overwrote"
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				verb = "Created"
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fail(fmt.Sprintf("cannot create the directory for %s: %v", in.Path, err))
			}
			if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
				return fail(fmt.Sprintf("cannot write %s: %v", in.Path, err))
			}
			return tool.Succeeded(fmt.Sprintf("%s %s (%d bytes).", verb, in.Path, len(in.Content))), nil
		},
	)
	return closedWorld(mutating(t), err)
}

// editMaxBytes bounds the file that edit_file loads into memory. Source
// files are far smaller.
const editMaxBytes = 32 << 20

type editArgs struct {
	Path       string `json:"path" description:"File to edit. Relative to the workspace, or absolute inside it."`
	OldString  string `json:"old_string" description:"The exact text to replace, with its whitespace. It must appear once, unless replace_all is true."`
	NewString  string `json:"new_string" description:"The replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace every occurrence of old_string."`
}

func newEditFile(s *scope) (tool.Tool, error) {
	t, err := tool.Func(
		ToolEditFile,
		"Replace text in a file. old_string must match the file exactly and "+
			"must appear once. If it appears more than once, add surrounding "+
			"lines to make it unique, or set replace_all. Read the file first.",
		func(_ context.Context, _ tool.Call, in editArgs) (tool.Outcome, error) {
			path, err := s.resolve(in.Path)
			if err != nil {
				return fail(err.Error())
			}
			info, err := os.Stat(path)
			switch {
			case errors.Is(err, os.ErrNotExist):
				return fail(fmt.Sprintf("file %s does not exist. Use write_file to create it.", in.Path))
			case err != nil:
				return fail(fmt.Sprintf("cannot read %s: %v", in.Path, err))
			case info.IsDir():
				return fail(fmt.Sprintf("%s is a directory. Give a file path.", in.Path))
			case !info.Mode().IsRegular():
				// Opening a named pipe or a device can block forever.
				return fail(fmt.Sprintf("%s is not a regular file. The edit_file tool edits regular files only.", in.Path))
			case info.Size() > editMaxBytes:
				return fail(fmt.Sprintf(
					"%s is %d bytes, more than the %d bytes that edit_file accepts.",
					in.Path, info.Size(), editMaxBytes,
				))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fail(fmt.Sprintf("cannot read %s: %v", in.Path, err))
			}
			updated, count, err := applyEdit(in.Path, string(data), in)
			if err != nil {
				return fail(err.Error())
			}
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				return fail(fmt.Sprintf("cannot write %s: %v", in.Path, err))
			}
			return tool.Succeeded(fmt.Sprintf("Edited %s: replaced %d occurrence(s).", in.Path, count)), nil
		},
	)
	return closedWorld(mutating(t), err)
}

// applyEdit returns text with the edit applied and the number of replacements.
// Its errors are written for the model.
func applyEdit(name, text string, in editArgs) (string, int, error) {
	if in.OldString == "" {
		return "", 0, errors.New("old_string is empty. Give the text to replace, or use write_file to write a whole file.")
	}
	if in.OldString == in.NewString {
		return "", 0, errors.New("old_string and new_string are the same. Nothing would change.")
	}
	count := strings.Count(text, in.OldString)
	if count == 0 && strings.Contains(in.OldString, "\n") && strings.Contains(text, "\r\n") {
		// read_file shows lines without their \r, so the model writes \n.
		// Match and write the file's CRLF line ends instead.
		crlf := strings.NewReplacer("\r\n", "\r\n", "\n", "\r\n")
		in.OldString, in.NewString = crlf.Replace(in.OldString), crlf.Replace(in.NewString)
		count = strings.Count(text, in.OldString)
	}
	switch {
	case count == 0:
		return "", 0, fmt.Errorf("old_string not found in %s. Read the file again and copy the text exactly, with its whitespace.", name)
	case count > 1 && !in.ReplaceAll:
		return "", 0, fmt.Errorf(
			"old_string appears %d times (lines %s). Add surrounding lines to make it unique, or set replace_all.",
			count,
			matchLines(text, in.OldString),
		)
	}
	if !in.ReplaceAll {
		count = 1
	}
	return strings.Replace(text, in.OldString, in.NewString, count), count, nil
}

// matchLines lists the line numbers where old starts, at most ten.
func matchLines(text, old string) string {
	var lines []string
	pos, line := 0, 1
	for len(lines) < 10 {
		i := strings.Index(text[pos:], old)
		if i < 0 {
			break
		}
		line += strings.Count(text[pos:pos+i], "\n")
		lines = append(lines, fmt.Sprint(line))
		pos += i + len(old)
		line += strings.Count(old, "\n")
	}
	out := strings.Join(lines, ", ")
	if strings.Count(text, old) > len(lines) {
		out += ", ..."
	}
	return out
}
