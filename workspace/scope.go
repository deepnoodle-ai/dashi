package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scope holds the workspace root and decides which paths a tool may touch.
type scope struct {
	root string // absolute, symlinks resolved
}

func newScope(dir string) (*scope, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve directory %q: %w", dir, err)
	}
	root, err := resolveLinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve directory %q: %w", dir, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %q is not a directory", dir)
	}
	return &scope{root: root}, nil
}

// resolve returns the absolute path for p with symlinks resolved. p may name
// a file that does not exist yet. The result lies inside the workspace, or
// resolve returns an error that the model can act on. Callers must use the
// returned path, not p, so that a link cannot change the target between the
// check and the use. The path is cleaned before links are read, so "link/.." names the
// directory that holds link. The tools open that same cleaned path.
func (s *scope) resolve(p string) (string, error) {
	if p == "" {
		return s.root, nil
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(s.root, p)
	}
	resolved, err := resolveLinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %v", p, err)
	}
	if !s.contains(resolved) {
		return "", fmt.Errorf(
			"path %q is outside the workspace %s. Use a path inside it.",
			p,
			s.root,
		)
	}
	return resolved, nil
}

func (s *scope) contains(path string) bool {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// rel shows path relative to the root, for messages to the model.
func (s *scope) rel(path string) string {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

const maxLinks = 40

// resolveLinks resolves every symlink in the existing part of an absolute,
// clean path. The rest of the path, which does not exist yet, is appended
// unchanged. A dangling link is followed, so a link that points to a new file
// outside the workspace is caught.
func resolveLinks(path string) (string, error) {
	links := 0
	for {
		volume := filepath.VolumeName(path)
		rest := strings.TrimPrefix(path[len(volume):], string(filepath.Separator))
		cur := volume + string(filepath.Separator)
		parts := strings.Split(rest, string(filepath.Separator))
		restart := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := filepath.Join(cur, part)
			info, err := os.Lstat(next)
			if errors.Is(err, os.ErrNotExist) {
				return filepath.Join(append([]string{cur}, parts[i:]...)...), nil
			}
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				cur = next
				continue
			}
			if links++; links > maxLinks {
				return "", errors.New("too many symbolic links")
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(cur, target)
			}
			path = filepath.Join(append([]string{filepath.Clean(target)}, parts[i+1:]...)...)
			restart = true
			break
		}
		if !restart {
			return cur, nil
		}
	}
}
