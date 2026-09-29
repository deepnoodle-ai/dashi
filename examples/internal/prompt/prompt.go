// Package prompt reads answers typed at the terminal, for the examples. A
// read ends with its context, so Ctrl-C stops a program waiting for an
// answer.
package prompt

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Input reads lines on its own goroutine. Create one per program: two would
// race for the same lines.
type Input struct {
	lines chan string
}

// Stdin returns an Input reading standard input.
func Stdin() *Input {
	return New(os.Stdin)
}

// New returns an Input reading r.
func New(r io.Reader) *Input {
	in := &Input{lines: make(chan string)}
	go func() {
		defer close(in.lines)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			in.lines <- scanner.Text()
		}
	}()
	return in
}

// Lines returns the lines as they are typed, for a select. It is closed at
// end of input.
func (in *Input) Lines() <-chan string {
	return in.lines
}

// Ask prints question and returns the next line with surrounding space
// removed: "" at end of input, or ctx's error on Ctrl-C.
func (in *Input) Ask(ctx context.Context, question string) (string, error) {
	fmt.Print(question)
	select {
	case line := <-in.lines:
		return strings.TrimSpace(line), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Confirm asks a yes-or-no question and reports whether the answer was y.
// Any other answer, and end of input, is no.
func (in *Input) Confirm(ctx context.Context, question string) (bool, error) {
	line, err := in.Ask(ctx, question+" [y/N] ")
	return strings.EqualFold(line, "y"), err
}
