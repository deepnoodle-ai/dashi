//go:build unix

package workspace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcess starts the shell in its own session. The session has no
// terminal, so a command cannot prompt on it. The process group lets a
// timeout kill the shell and every process that it started.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
