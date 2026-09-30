package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeClipboardUsesStdinAndBoundedProcesses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fixture replaces pbcopy")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "captured")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DASHI_CLIPBOARD_FIXTURE", capture)
	binary := filepath.Join(dir, "pbcopy")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat > \"$DASHI_CLIPBOARD_FIXTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	source := "日本 long line\n$(this is literal) `also literal`"
	if _, err := writeClipboard(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != source {
		t.Fatalf("clipboard source changed: %q %v", got, err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := writeClipboard(ctx, source); err == nil {
		t.Fatal("hanging clipboard claimed success")
	}
	if time.Since(start) > time.Second {
		t.Fatal("clipboard process outlived its bounded context")
	}
}

func TestClipboardFallbackIsExplicitBoundedAndHonest(t *testing.T) {
	var transport bytes.Buffer
	s := newScreen("fixture")
	s.clipboardOut = &transport
	s.clipboardResult(clipboardResultEvent{text: "世界", err: context.DeadlineExceeded})
	s.drain()
	if !strings.Contains(transport.String(), "\x1b]52;") || !strings.Contains(s.tr.Blocks[0].Text, "unconfirmed") {
		t.Fatal("OSC52 receipt was overstated or request omitted")
	}
	transport.Reset()
	s.clipboardResult(clipboardResultEvent{text: strings.Repeat("x", 100*1024+1), err: context.DeadlineExceeded})
	s.drain()
	if transport.Len() != 0 || !strings.Contains(s.tr.Blocks[len(s.tr.Blocks)-1].Text, "/scrollback") {
		t.Fatal("oversized OSC52 did not offer safe fallback")
	}
}
