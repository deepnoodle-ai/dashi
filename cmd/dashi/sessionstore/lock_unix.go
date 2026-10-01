//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package sessionstore

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking flock on f. It reports false when
// another open file description holds the lock.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}
