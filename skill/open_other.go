//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package skill

import "os"

func openDefinition(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
