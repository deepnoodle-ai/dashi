//go:build !(linux || darwin || freebsd || openbsd || netbsd || android || ios)

package filerecorder

import (
	"context"
	"errors"
	"os"
)

// supported is false: this system has no flock in the standard library, and
// a recorder without cross-process locking would let two writers interleave.
const supported = false

func lock(context.Context, *os.File, bool) error {
	return errors.New("filerecorder: file locking is not supported")
}
