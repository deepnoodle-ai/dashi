//go:build linux

package sessionstore

import "golang.org/x/sys/unix"

const attachmentFDPath = "/proc/self/fd"

func (d *attachmentDir) publishUnused(from, to string) error {
	return unix.Renameat2(int(d.file.Fd()), from, int(d.file.Fd()), to, unix.RENAME_NOREPLACE)
}

func attachmentHostPath(path string) string { return path }
