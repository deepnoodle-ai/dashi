//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAutomationInvocationInheritedPipeFrames(t *testing.T) {
	for _, format := range []string{"json", "jsonl"} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAutomationRunProcessFixture$")
		cmd.Env = append(os.Environ(), "DASHI_AUTOMATION_RUN_FIXTURE="+format)
		cmd.Stdin = strings.NewReader("日本\tdata\n\n/quit\nyes\n@missing\n")
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s %v %s", format, err, diag.String())
		}
		got := decodeResponses(t, format, out.String())
		if len(got) != 1 || got[0].Content != "The test failed: expected 3, got 4.\n日本\u2028" {
			t.Fatal(out.String())
		}
	}
}
func TestAutomationInvocationInheritedTTYFramesAndUntouchedInput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python PTY fixture unavailable")
	}
	script := `import os, pty, subprocess, sys, termios, select, json, time
mi, si = pty.openpty()
mo, so = pty.openpty()
before_in, before_out = termios.tcgetattr(si), termios.tcgetattr(so)
literal = b"terminal input remains unread\n"
os.write(mi, literal)
env = dict(os.environ, DASHI_AUTOMATION_RUN_FIXTURE=sys.argv[2])
p = subprocess.Popen([sys.argv[1], "-test.run=^TestAutomationRunProcessFixture$"], stdin=si, stdout=so, stderr=subprocess.PIPE, env=env)
data = bytearray()
end = time.monotonic() + 5
while time.monotonic() < end:
    ready, _, _ = select.select([mo], [], [], .1)
    if ready: data.extend(os.read(mo, 65536))
    if p.poll() is not None:
        while select.select([mo], [], [], .01)[0]: data.extend(os.read(mo, 65536))
        break
assert p.wait(timeout=1) == 0, p.stderr.read()
assert before_in == termios.tcgetattr(si)
assert before_out == termios.tcgetattr(so)
assert os.read(si, len(literal)) == literal
frames = bytes(data).splitlines()
assert len(frames) == 1, data
obj = json.loads(frames[0])
if sys.argv[2] == "json":
    assert set(obj) == {"messages"}, obj
    obj = obj["messages"][0]
assert set(obj) == {"content"}, obj
assert obj["content"] == "The test failed: expected 3, got 4.\n日本\u2028", obj
sys.stdout.buffer.write(data)
for fd in (mi, si, mo, so): os.close(fd)
`
	for _, format := range []string{"json", "jsonl"} {
		ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
		cmd := exec.CommandContext(ctx, python, "-c", script, os.Args[0], format)
		data, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s %v %s", format, err, data)
		}
		decodeResponses(t, format, string(data))
	}
}
func TestAutomationInvocationSignalsCancelWholeInputBeforeAcceptance(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutomationRunProcessFixture$")
		cmd.Env = append(os.Environ(), "DASHI_AUTOMATION_RUN_FIXTURE=signal-input")
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stderr)
		if !scanner.Scan() || scanner.Text() != "ready" {
			t.Fatal("fixture did not start")
		}
		// The child enters the read after qualification. A held input pipe prevents
		// either live setup or process completion during signal-handler registration.
		time.Sleep(100 * time.Millisecond)
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
		}
		err = cmd.Wait()
		in.Close()
		cancel()
		exit, ok := err.(*exec.ExitError)
		want := 128 + int(sig)
		if !ok || exit.ExitCode() != want {
			t.Fatalf("signal=%v error=%v", sig, err)
		}
		var object responseMessages
		if err := json.Unmarshal(out.Bytes(), &object); err != nil || len(object.Messages) != 0 {
			t.Fatalf("signal=%v out=%q err=%v", sig, out.String(), err)
		}
		fmt.Fprintf(os.Stderr, "signal %d: exit %d, empty response, no admission\n", sig, want)
	}
}
