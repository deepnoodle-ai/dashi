//go:build !(linux || darwin || freebsd || openbsd || netbsd || android || ios)

package sessionstore

import "os"

// tryLock always succeeds: this system has no flock in the standard library,
// so a second process on one session is not detected.
func tryLock(*os.File) (bool, error) {
	return true, nil
}
