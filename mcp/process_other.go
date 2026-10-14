//go:build !unix && !windows

package mcp

import "os/exec"

// ownGroup does nothing on systems without process groups.
func ownGroup(*exec.Cmd) {}

// stopGroup does nothing on systems without process groups.
func stopGroup(*exec.Cmd) {}
