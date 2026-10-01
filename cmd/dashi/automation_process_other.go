//go:build !darwin && !linux

package main

import (
	"errors"
	"os"

	"golang.org/x/term"
)

func automationProcessTerminal(file *os.File) (bool, error) {
	return term.IsTerminal(int(file.Fd())), nil
}

// Other platforms retain finite files and terminal-input skipping. Pipe
// execution refuses before dispatch until that platform has qualified proof.
func openAutomationProcessPipe(*os.File, bool) (automationProcessPipeIO, error) {
	return nil, errors.New("process pipes are unsupported on this platform")
}
