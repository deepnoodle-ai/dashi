//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package mention

import (
	"os"
	"syscall"
)

func openSelection(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
