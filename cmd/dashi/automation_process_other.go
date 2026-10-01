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
// and terminal-output execution refuse before dispatch until that platform
// has qualified proof.
func openAutomationProcessDescriptor(*os.File, bool) (automationProcessDescriptorIO, error) {
	return nil, errors.New("process pipe/terminal output is unsupported on this platform")
}
