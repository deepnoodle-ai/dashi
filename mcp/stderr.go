package mcp

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
)

// Limits on the stderr text that a failed status carries.
const (
	tailLines = 20
	tailBytes = 4096
)

// tail keeps the last lines of a child's stderr, so a start failure can say
// why. It holds at most tailBytes bytes, so a noisy server cannot grow it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

// Write implements io.Writer and never fails, so it cannot stop the child.
func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - tailBytes; extra > 0 {
		t.buf = t.buf[extra:]
	}
	return len(p), nil
}

// text returns the last tailLines lines.
func (t *tail) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(string(bytes.TrimSpace(t.buf)), "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// explain adds the stderr text to err. A nil tail leaves err unchanged.
func (t *tail) explain(err error) error {
	if t == nil {
		return err
	}
	text := t.text()
	if text == "" {
		return err
	}
	return fmt.Errorf("%w; server stderr:\n%s", err, text)
}
