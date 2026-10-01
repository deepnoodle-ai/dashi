//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func automationDescriptorFlags(file *os.File) (int, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var flags int
	var flagErr error
	err = conn.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) })
	if err != nil {
		return 0, err
	}
	return flags, flagErr
}

// Darwin can report a private written-status bit after Write. Compare the
// configurable status/access flags; kernel-maintained history is not a mode.
func automationConfigFlags(flags int) int {
	return flags & (unix.O_ACCMODE | unix.O_NONBLOCK | unix.O_APPEND | unix.O_ASYNC | unix.O_SYNC | unix.O_DSYNC)
}

// The subprocess receives real inherited blocking fd 0/fd 1. Exit explicitly
// to keep the test runner's PASS banner out of the machine-output fixture.
func TestAutomationProcessChild(t *testing.T) {
	mode := os.Getenv("DASHI_AUTOMATION_PROCESS_FIXTURE")
	if mode == "" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(7) }
	beforeIn, err := automationDescriptorFlags(os.Stdin)
	if err != nil {
		fail(err)
	}
	beforeOut, err := automationDescriptorFlags(os.Stdout)
	if err != nil {
		fail(err)
	}
	in, closeIn, err := prepareAutomationProcessInput(os.Stdin)
	if err != nil {
		fail(err)
	}
	out, closeOut, err := prepareAutomationProcessOutput(os.Stdout)
	if mode == "broken" {
		// A known broken pipe can refuse during qualification or fail on its
		// first checked write. Both leave execution unstarted.
		if err == nil {
			o, e := newAutomationOutput(context.Background(), "json", out)
			if e != nil {
				fail(e)
			}
			if e = finishAutomationContent(o, ""); e == nil {
				fail(errors.New("broken pipe succeeded"))
			}
		}
		if closeOut != nil {
			if e := closeOut.Close(); e != nil {
				fail(e)
			}
		}
		if closeIn != nil {
			if e := closeIn.Close(); e != nil {
				fail(e)
			}
		}
		after, _ := automationDescriptorFlags(os.Stdout)
		if after != beforeOut {
			fail(errors.New("stdout flags changed"))
		}
		os.Exit(0)
	}
	if err != nil {
		fail(err)
	}
	writer, err := qualifyAutomationWriter(out)
	if err != nil {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "ready")
	var answer string
	switch mode {
	case "terminal":
		if in != nil || closeIn != nil {
			fail(errors.New("terminal stdin was selected for reading"))
		}
		answer = "terminal unread"
	case "read", "input-cancel":
		ctx := context.Background()
		cancel := func() {}
		if mode == "input-cancel" {
			ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
		}
		data, readErr := readAutomationStdin(ctx, in, closeIn)
		cancel()
		if mode == "input-cancel" {
			if !errors.Is(readErr, context.DeadlineExceeded) || data != "" {
				fail(errors.New("stdin cancellation failed"))
			}
			// Cancellation accepts no input.
		} else if readErr != nil {
			fail(readErr)
		} else {
			answer = data
		}
	case "output-cancel", "output-stall", "slow":
		answer = strings.Repeat("日本\n", modeSize(mode))
	default:
		fail(errors.New("unknown fixture"))
	}
	ctx := context.Background()
	cancel := func() {}
	if mode == "output-cancel" {
		ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
	}
	o := startAutomationOutput(ctx, "json", writer, 100*time.Millisecond)
	writeErr := finishAutomationContent(o, answer)
	cancel()
	if mode == "output-cancel" || mode == "output-stall" {
		if writeErr == nil {
			fail(errors.New("stalled stdout succeeded"))
		}
	} else if writeErr != nil {
		fail(writeErr)
	}
	if closeIn != nil {
		if e := closeIn.Close(); e != nil {
			fail(e)
		}
	}
	if closeOut != nil {
		if e := closeOut.Close(); e != nil {
			fail(e)
		}
	}
	afterIn, _ := automationDescriptorFlags(os.Stdin)
	afterOut, _ := automationDescriptorFlags(os.Stdout)
	if automationConfigFlags(beforeIn) != automationConfigFlags(afterIn) || automationConfigFlags(beforeOut) != automationConfigFlags(afterOut) {
		fail(fmt.Errorf("inherited descriptor flags changed: stdin %x -> %x; stdout %x -> %x", beforeIn, afterIn, beforeOut, afterOut))
	}
	if _, e := os.Stdin.Stat(); e != nil {
		fail(errors.New("original stdin closed"))
	}
	if _, e := os.Stdout.Stat(); e != nil {
		fail(errors.New("original stdout closed"))
	}
	os.Exit(0)
}

func modeSize(mode string) int {
	if mode == "slow" {
		return 12000
	}
	return 400000
}

func automationProcessCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutomationProcessChild$")
	cmd.Env = append(os.Environ(), "DASHI_AUTOMATION_PROCESS_FIXTURE="+mode)
	return cmd
}

func TestAutomationInheritedInputAndOneFinal(t *testing.T) {
	for _, mode := range []string{"read", "input-cancel"} {
		t.Run(mode, func(t *testing.T) {
			cmd := automationProcessCommand(t, mode)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			literal := "日本\tline\n\n/quit\nyes\n@missing\n"
			if mode == "read" {
				if _, err := io.WriteString(stdin, literal); err != nil {
					t.Fatal(err)
				}
				stdin.Close()
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			if bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
				t.Fatalf("framing=%q", stdout.String())
			}
			var result responseMessages
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Messages) > 1 {
				t.Fatalf("result=%+v", result)
			}
			if mode == "read" && responseContent(result) != literal {
				t.Fatalf("literal=%q", responseContent(result))
			}
			if mode == "input-cancel" && (responseContent(result) != "") {
				t.Fatalf("canceled=%+v", result)
			}
		})
	}
}

func TestAutomationInheritedOutputStallCancelAndBroken(t *testing.T) {
	for _, mode := range []string{"output-stall", "output-cancel", "broken"} {
		t.Run(mode, func(t *testing.T) {
			cmd := automationProcessCommand(t, mode)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if mode == "broken" {
				stdout.Close()
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Deliberately do not consume stdout. The process must stop without
			// relying on a reader, retrying a result, or leaking a worker.
			if err := cmd.Wait(); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
		})
	}
}

func TestAutomationInheritedHealthySlowOutput(t *testing.T) {
	cmd := automationProcessCommand(t, "slow")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("ready=%q %v", line, err)
	}
	var data bytes.Buffer
	buf := make([]byte, 1024)
	for {
		n, err := stdout.Read(buf)
		data.Write(buf[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data.Bytes(), []byte("\n")) != 1 {
		t.Fatal("more than one final envelope")
	}
	var result responseMessages
	if err := json.Unmarshal(data.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(responseContent(result)) != len("日本\n")*modeSize("slow") {
		t.Fatal("short final output")
	}
}

func TestAutomationProcessRegularFilesAndRefusal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out, closer, err := prepareAutomationProcessOutput(file)
	if err != nil || closer != nil {
		t.Fatalf("regular=%v %v", closer, err)
	}
	o, err := newAutomationOutput(t.Context(), "json", out)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishAutomationContent(o, "saved"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("original regular file closed")
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if _, _, err := prepareAutomationProcessInput(dir); err == nil {
		t.Fatal("directory stdin accepted")
	}
	if _, _, err := prepareAutomationProcessOutput(dir); err == nil {
		t.Fatal("directory stdout accepted")
	}
}

func TestAutomationInheritedTerminalInputUntouched(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python PTY fixture unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	script := `import os, pty, subprocess, sys, termios
master, slave = pty.openpty()
before = termios.tcgetattr(slave)
payload = b"terminal input stays here\n"
os.write(master, payload)
env = dict(os.environ, DASHI_AUTOMATION_PROCESS_FIXTURE="terminal")
p = subprocess.run([sys.argv[1], "-test.run=^TestAutomationProcessChild$"], stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, timeout=4)
assert p.returncode == 0, p.stderr
assert before == termios.tcgetattr(slave)
assert os.read(slave, len(payload)) == payload
sys.stdout.buffer.write(p.stdout)
os.close(slave)
os.close(master)
`
	cmd := exec.CommandContext(ctx, python, "-c", script, os.Args[0])
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PTY fixture: %v: %s", err, data)
	}
	var result responseMessages
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if responseContent(result) != "terminal unread" {
		t.Fatalf("result=%+v", result)
	}
}
