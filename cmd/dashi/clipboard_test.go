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

// fixtureClipboardTimeout replaces clipboardTimeout in these tests. On a
// loaded machine, starting a freshly written fixture script can take longer
// than the production bound; these tests check what is written, not how fast.
const fixtureClipboardTimeout = time.Minute

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
	if result, err := writeClipboardWithin(t.Context(), source, fixtureClipboardTimeout); err != nil || !result.confirmed {
		t.Fatalf("local write result=%+v error=%v", result, err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != source {
		t.Fatalf("clipboard source changed: %q %v", got, err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := writeClipboardWithin(ctx, source, fixtureClipboardTimeout); err == nil {
		t.Fatal("hanging clipboard claimed success")
	}
	// Well under the fixture's sleep, with room for a slow process start.
	if time.Since(start) > 10*time.Second {
		t.Fatal("clipboard process outlived its bounded context")
	}
}

func TestClipboardFallbackIsExplicitBoundedAndHonest(t *testing.T) {
	var transport bytes.Buffer
	s := newScreen("fixture")
	s.clipboardOut = &transport
	s.clipboardResult(clipboardResultEvent{text: "世界", err: context.DeadlineExceeded})
	s.drain()
	if !strings.Contains(transport.String(), "\x1b]52;") || !strings.Contains(s.main().tr.Blocks[0].Text, "unconfirmed") {
		t.Fatal("OSC52 receipt was overstated or request omitted")
	}
	transport.Reset()
	s.clipboardResult(clipboardResultEvent{text: strings.Repeat("x", 100*1024+1), err: context.DeadlineExceeded})
	s.drain()
	if transport.Len() != 0 || !strings.Contains(s.main().tr.Blocks[len(s.main().tr.Blocks)-1].Text, "/scrollback") {
		t.Fatal("oversized OSC52 did not offer safe fallback")
	}
}

func TestTmuxClipboardDoesNotClaimConfirmedReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a shell command")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "captured")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "fixture")
	t.Setenv("DASHI_CLIPBOARD_FIXTURE", capture)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\ncat > \"$DASHI_CLIPBOARD_FIXTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const source = "世界\nexact source"
	result, err := writeClipboardWithin(t.Context(), source, fixtureClipboardTimeout)
	if err != nil || result.confirmed || !strings.Contains(result.notice, "unconfirmed") {
		t.Fatalf("tmux result=%+v error=%v", result, err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != source {
		t.Fatalf("tmux source=%q error=%v", got, err)
	}
}
