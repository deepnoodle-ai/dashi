//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package main

import (
	"errors"
	"os"
	"syscall"
)

func preferenceFileFlags() (int, error) { return syscall.O_NOFOLLOW | syscall.O_NONBLOCK, nil }
func lockPreferences(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("user settings are busy; retry")
		}
		return errors.New("cannot lock user settings")
	}
	return nil // closing the file releases the lock
}
