//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd as the leader of a new process group, so a terminal
// Ctrl-C sent to the host's group does not reach it.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// stopGroup kills what is left of cmd's process group. The leader has
// already exited or been killed; this reaches children it left behind.
func stopGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// An error means the group is already gone.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
