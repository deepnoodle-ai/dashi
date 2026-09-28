//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// supported reports whether this system has flock. The build constraint
// names each system instead of using "unix", which also covers systems
// without syscall.Flock.
const supported = true

// Lock waits are bounded by the context rather than blocking in the kernel:
// the runner gives a commit after cancellation only a few seconds.
const (
	minLockDelay = time.Millisecond
	maxLockDelay = 20 * time.Millisecond
)

// lock takes an exclusive or shared flock on f, retrying a non-blocking
// attempt until it succeeds or ctx ends. A flock belongs to the open file
// description, so two opens of one log exclude each other even within one
// process; fcntl locks belong to the process and would not. Closing f
// releases the lock.
func lock(ctx context.Context, f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	fd := int(f.Fd())
	delay := minLockDelay
	for {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) &&
			!errors.Is(err, syscall.EINTR) {
			return &os.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
		delay = min(2*delay, maxLockDelay)
	}
}
