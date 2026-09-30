//go:build !(linux || darwin || freebsd || openbsd || netbsd || android || ios)

package main

import (
	"errors"
	"os"
)

func preferenceFileFlags() (int, error) {
	return 0, errors.New("saving user settings requires platform advisory locks")
}
func lockPreferences(*os.File) error {
	return errors.New("saving user settings requires platform advisory locks")
}
