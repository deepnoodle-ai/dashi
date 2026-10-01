//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Exercise the actual inherited stdout descriptor without an execution report
// or any model/tool dispatch. Payloads only test checked byte delivery.
func TestAutomationTTYProcessChild(t *testing.T) {
	mode := os.Getenv("DASHI_AUTOMATION_TTY_FIXTURE")
	if mode == "" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(7) }
	before, err := automationDescriptorFlags(os.Stdout)
	if err != nil {
		fail(err)
	}
	out, closer, err := prepareAutomationProcessOutput(os.Stdout)
	if mode == "broken-before" {
		if err == nil {
			fail(errors.New("disconnected terminal passed qualification"))
		}
	} else {
		if err != nil {
			fail(err)
		}
		in, closeIn, err := prepareAutomationProcessInput(os.Stdin)
		if err != nil || in != nil || closeIn != nil {
			fail(errors.New("terminal stdin was selected for reading"))
		}
		writer, err := qualifyAutomationWriter(out)
		if err != nil {
			fail(err)
		}
		ctx := context.Background()
		cancel := func() {}
		if mode == "cancel" {
			ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
		}
		content := "日本\n\t"
		if mode == "slow" {
			content = strings.Repeat("日本\n", 12000)
		} else if mode != "json" && mode != "jsonl" {
			content = strings.Repeat("日本\n", 400000)
		}
		data, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			fail(err)
		}
		data = append(data, '\n')
		if mode == "jsonl" {
			data = append(append([]byte(nil), data...), data...)
		}
		fmt.Fprintln(os.Stderr, "ready")
		started := time.Now()
		err = writer.write(ctx, data, 100*time.Millisecond)
		cancel()
		if mode == "stall" || mode == "cancel" || mode == "broken-during" {
			if err == nil || time.Since(started) > time.Second {
				fail(errors.New("terminal output did not stop promptly"))
			}
		} else if err != nil {
			fail(err)
		}
		if err := closer.Close(); err != nil {
			fail(err)
		}
	}
	after, err := automationDescriptorFlags(os.Stdout)
	if err != nil || automationConfigFlags(before) != automationConfigFlags(after) {
		fail(errors.New("original stdout flags changed or descriptor closed"))
	}
	if _, err := os.Stdin.Stat(); err != nil {
		fail(errors.New("original stdin closed"))
	}
	fmt.Fprintln(os.Stderr, "finished")
	os.Exit(0)
}

func TestAutomationInheritedTTYOutput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python PTY fixture unavailable")
	}
	const script = `import os, pty, subprocess, sys, termios, fcntl, select, json, time
mode = sys.argv[2]
master, slave = pty.openpty()
flags = fcntl.fcntl(slave, fcntl.F_GETFL)
if mode == "jsonl":
    fcntl.fcntl(slave, fcntl.F_SETFL, flags | os.O_NONBLOCK | os.O_APPEND)
before_flags = fcntl.fcntl(slave, fcntl.F_GETFL)
before_attrs = termios.tcgetattr(slave)
payload = b"terminal stdin remains unread\n"
os.write(master, payload)
assert select.select([master], [], [], 1)[0], "missing input echo"
os.read(master, 4096)  # Remove echo, leaving canonical input queued on the slave.
if mode == "broken-before":
    os.close(master)
env = dict(os.environ, DASHI_AUTOMATION_TTY_FIXTURE=mode)
p = subprocess.Popen([sys.argv[1], "-test.run=^TestAutomationTTYProcessChild$"], stdin=slave, stdout=slave, stderr=subprocess.PIPE, env=env)
def status():
    assert select.select([p.stderr], [], [], 2)[0], "child status timed out"
    return p.stderr.readline()
output = bytearray()
try:
    first = status()
    if mode == "broken-before":
        assert first == b"finished\n", first
    else:
        assert first == b"ready\n", first
        if mode == "broken-during":
            os.close(master)
        if mode in ("stall", "cancel", "broken-during"):
            assert status() == b"finished\n"
        else:
            deadline = time.monotonic() + 3
            while True:
                assert time.monotonic() < deadline, "healthy output timed out"
                ready = select.select([master, p.stderr], [], [], .1)[0]
                if master in ready:
                    output.extend(os.read(master, 1024))
                    if mode == "slow": time.sleep(.001)
                if p.stderr in ready:
                    assert p.stderr.readline() == b"finished\n"
                    while select.select([master], [], [], 0)[0]:
                        output.extend(os.read(master, 65536))
                    break
    assert p.wait(timeout=3) == 0, p.stderr.read()
    mask = os.O_ACCMODE | os.O_NONBLOCK | os.O_APPEND | getattr(os, "O_ASYNC", 0) | os.O_SYNC | getattr(os, "O_DSYNC", 0)
    assert before_flags & mask == fcntl.fcntl(slave, fcntl.F_GETFL) & mask
    if mode not in ("broken-before", "broken-during"):
        assert before_attrs == termios.tcgetattr(slave), "terminal state changed"
        assert os.read(slave, len(payload)) == payload, "terminal stdin consumed"
    if mode in ("json", "jsonl", "slow"):
        assert output[-1:] == b"\n", "unterminated frame"
        lines = output.split(b"\n")[:-1]
        assert len(lines) == (2 if mode == "jsonl" else 1), len(lines)
        for line in lines:
            value = json.loads(line)
            assert value["content"] == ("日本\n" * 12000 if mode == "slow" else "日本\n\t")
finally:
    if p.poll() is None: p.kill(); p.wait()
    if mode not in ("broken-before", "broken-during"): os.close(master)
    os.close(slave)
`
	for _, mode := range []string{"json", "jsonl", "slow", "stall", "cancel", "broken-before", "broken-during"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-c", script, os.Args[0], mode)
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("inherited PTY: %v: %s", err, data)
			}
		})
	}
}
