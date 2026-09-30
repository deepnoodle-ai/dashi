//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package session

import "os"

func openResourceFile(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
