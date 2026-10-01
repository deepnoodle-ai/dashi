//go:build darwin

package sessionstore

import (
	"golang.org/x/sys/unix"
	"strings"
)

const attachmentFDPath = "/dev/fd"

func (d *attachmentDir) publishUnused(from, to string) error {
	return unix.RenameatxNp(int(d.file.Fd()), from, int(d.file.Fd()), to, unix.RENAME_EXCL)
}

func attachmentHostPath(path string) string {
	// macOS exposes these system directories through fixed top-level aliases.
	for _, prefix := range []string{"/var/", "/tmp/"} {
		if strings.HasPrefix(path, prefix) {
			return "/private" + path
		}
	}
	return path
}
