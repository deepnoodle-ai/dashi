//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a new process group, so a console Ctrl-C sent to
// the host does not reach it.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// stopGroup does nothing on Windows: a process group there is not a unit
// that can be killed. The SDK's shutdown still stops the direct child.
func stopGroup(*exec.Cmd) {}
