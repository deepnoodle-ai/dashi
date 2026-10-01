//go:build !(linux || darwin || freebsd || openbsd || netbsd || android || ios)

package filerecorder

func readerFileFlags() int { return 0 }
