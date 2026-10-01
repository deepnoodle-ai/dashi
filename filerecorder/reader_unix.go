//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import "syscall"

func readerFileFlags() int { return syscall.O_NOFOLLOW | syscall.O_NONBLOCK }
