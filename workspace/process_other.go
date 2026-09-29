//go:build !unix

package workspace

import "os/exec"

// configureProcess keeps the default cancel, which kills the shell process
// only. Windows process-tree kill is later work.
func configureProcess(_ *exec.Cmd) {}
