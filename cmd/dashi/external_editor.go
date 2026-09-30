package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"
)

// editorArgv accepts quoted arguments, but never invokes a shell or expands text.
func editorArgv(value string) ([]string, error) {
	var args []string
	var token strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range value {
		if escaped {
			token.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				token.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				args = append(args, token.String())
				token.Reset()
				started = false
			}
			continue
		}
		if strings.ContainsRune("|;&<>`", r) {
			return nil, errors.New("editor command must contain an executable and arguments, not shell operators")
		}
		token.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("editor command has an unfinished quote or escape")
	}
	if started {
		args = append(args, token.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("editor command has no executable")
	}
	return args, nil
}
func selectedEditor() string {
	if value := strings.TrimSpace(os.Getenv("VISUAL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("EDITOR")); value != "" {
		return value
	}
	return "vi"
}
func prepareEditor(dir, draft, editor string) (path string, run func() error, cleanup func(), err error) {
	args, err := editorArgv(editor)
	if err != nil {
		return "", nil, nil, err
	}
	file, err := os.CreateTemp("", "dashi-draft-*")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create draft: %w", err)
	}
	cleanup = func() { os.Remove(file.Name()) }
	if _, err = file.WriteString(draft); err != nil {
		file.Close()
		cleanup()
		return "", nil, nil, fmt.Errorf("write draft: %w", err)
	}
	if err = file.Close(); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("close draft: %w", err)
	}
	return file.Name(), func() error {
		cmd := exec.Command(args[0], append(args[1:], file.Name())...)
		cmd.Dir = dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stdout
		return cmd.Run()
	}, cleanup, nil
}
func readEditedDraft(path string) (string, error) {
	flags, err := preferenceFileFlags()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|flags, 0)
	if err != nil {
		return "", fmt.Errorf("read edited draft: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("edited draft must be a regular file")
	}
	if err := file.Chmod(0600); err != nil {
		return "", errors.New("edited draft permissions could not be secured")
	}
	text, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return "", fmt.Errorf("read edited draft: %w", err)
	}
	if len(text) > 16<<20 {
		return "", errors.New("edited draft exceeds 16 MiB")
	}
	if !utf8.Valid(text) {
		return "", errors.New("edited draft is not valid UTF-8")
	}
	if strings.ContainsRune(string(text), 0) {
		return "", errors.New("edited draft contains a NUL character")
	}
	return string(text), nil
}
