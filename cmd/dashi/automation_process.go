package main

import (
	"errors"
	"io"
	"os"
	"time"
)

// Process adapters own duplicate pipe or terminal descriptors. The controller must
// use the adapted stream exclusively, then close it after its worker joins.
// Originals stay open. Regular files retain checked synchronous operations.
type automationProcessDescriptorIO interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

func prepareAutomationProcessInput(in *os.File) (io.Reader, io.Closer, error) {
	if in == nil {
		return nil, nil, nil
	}
	info, err := in.Stat()
	if err != nil {
		return nil, nil, errors.New("cannot inspect stdin")
	}
	terminal, err := automationProcessTerminal(in)
	if err != nil {
		return nil, nil, err
	}
	if terminal {
		return nil, nil, nil
	}
	if info.Mode().IsRegular() || automationProcessNull(info) {
		return in, nil, nil
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return nil, nil, errors.New("unsupported stdin descriptor")
	}
	pipe, err := openAutomationProcessDescriptor(in, false)
	if err != nil {
		return nil, nil, err
	}
	return pipe, pipe, nil
}

func prepareAutomationProcessOutput(out *os.File) (io.Writer, io.Closer, error) {
	if out == nil {
		return nil, nil, errors.New("stdout is unavailable")
	}
	info, err := out.Stat()
	if err != nil {
		return nil, nil, errors.New("cannot inspect stdout")
	}
	if info.Mode().IsRegular() {
		return out, nil, nil
	}
	if automationProcessNull(info) {
		return automationNullOutput{out}, nil, nil
	}
	terminal, err := automationProcessTerminal(out)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeNamedPipe == 0 && !terminal {
		return nil, nil, errors.New("unsupported stdout descriptor")
	}
	stream, err := openAutomationProcessDescriptor(out, true)
	if err != nil {
		return nil, nil, err
	}
	return stream, stream, nil
}

func automationProcessNull(info os.FileInfo) bool {
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(info, null)
}

// The null device is a finite sink. This private wrapper cannot wrap an
// arbitrary writer and is created only after checking device identity.
type automationNullOutput struct{ file *os.File }

func (w automationNullOutput) Write(data []byte) (int, error) { return w.file.Write(data) }
