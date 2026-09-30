package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// writeClipboard uses bounded local processes. OSC52 is emitted separately
// on the terminal's owning loop because receipt cannot be confirmed.
func writeClipboard(ctx context.Context, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdin = strings.NewReader(text)
		cmd.WaitDelay = 200 * time.Millisecond
		return cmd.Run()
	}
	if os.Getenv("TMUX") != "" {
		if err := run("tmux", "load-buffer", "-w", "-"); err == nil {
			return "Copied to tmux; terminal clipboard receipt is unconfirmed.", nil
		}
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return "", errors.New("use the remote terminal clipboard")
	}
	switch runtime.GOOS {
	case "darwin":
		if err := run("pbcopy"); err == nil {
			return "Copied to the local clipboard.", nil
		}
	case "windows":
		if err := run("clip.exe"); err == nil {
			return "Copied to the local clipboard.", nil
		}
	case "linux":
		for _, command := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}} {
			if err := run(command[0], command[1:]...); err == nil {
				return "Copied to the local clipboard.", nil
			}
		}
	}
	return "", errors.New("local clipboard is unavailable")
}
