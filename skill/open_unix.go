//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package skill

import (
	"os"
	"syscall"
)

// Root prevents escaping links; nonblocking open refuses a substituted FIFO.
func openDefinition(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
