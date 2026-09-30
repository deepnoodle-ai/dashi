//go:build darwin || linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// attachmentDir pairs a pinned os.Root with no-follow descriptor operations.
// Only fixed, single-component names reach these helpers.
type attachmentDir struct {
	file *os.File
	root *os.Root
}

func rootedAttachmentDir(fd int) (*attachmentDir, error) {
	f := os.NewFile(uintptr(fd), "attachment directory")
	alias := "/dev/fd"
	if attachmentFDPath != "" {
		alias = attachmentFDPath
	}
	r, err := os.OpenRoot(fmt.Sprintf("%s/%d", alias, fd))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &attachmentDir{f, r}, nil
}

func openAttachmentAnchor(path string) (*attachmentDir, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Only fixed OS aliases are normalized. Every user-controlled store
	// component, including ancestors, is opened without following links.
	full = attachmentHostPath(full)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(full, "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	return rootedAttachmentDir(fd)
}

func (d *attachmentDir) close() { d.root.Close(); d.file.Close() }
func (d *attachmentDir) child(name string) (*attachmentDir, error) {
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return rootedAttachmentDir(fd)
}
func (d *attachmentDir) regular(name string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if create {
		flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, flags, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = fmt.Errorf("session: attachment must be a regular file")
		}
		return nil, err
	}
	return f, nil
}
