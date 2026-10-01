//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package mention

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSelectedFIFOIsRejectedWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadFileContext(context.Background(), path, Options{}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("selection blocked opening FIFO")
	}
}
