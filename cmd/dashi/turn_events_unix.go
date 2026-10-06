//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package main

import "syscall"

// turnEventsOpenFlags refuses a symlink at the --turn-events path, and
// O_NONBLOCK keeps a FIFO there from blocking the open, so openTurnEvents
// can reject it as a non-regular file.
const turnEventsOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
