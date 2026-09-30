//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package session

import (
	"os"
	"syscall"
)

// Refuse a replaced symlink and never block opening a substituted FIFO.
func openResourceFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
