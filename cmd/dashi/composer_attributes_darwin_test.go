package main

import "golang.org/x/sys/unix"

func composerTTYAttributes(fd int) (any, error) {
	attrs, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err == nil {
		// Darwin sets this transient reprocessing flag after raw-to-canonical return.
		// Match Wonton's qualification: compare modes, speeds, and control characters.
		attrs.Lflag &^= unix.PENDIN
	}
	return attrs, err
}
