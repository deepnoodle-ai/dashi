//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import "os"

func openResourceRead(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
