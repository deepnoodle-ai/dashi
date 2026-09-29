package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/mcp"
)

// TestServerProcess runs the example's real server in a separate process.
// The parent checks the PID after run returns to prove that Close reaped it.
func TestServerProcess(t *testing.T) {
	pidFile := os.Getenv("KEYLESS_TEST_PID_FILE")
	if pidFile == "" {
		return
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	if err := serve(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0) // Do not write the test harness's output to the protocol stream.
}

const transcript = "MCP server demo: connected (1 tool)\n" +
	"MCP result: {\"sum\":5}\n" +
	"Scripted answer: 2 + 3 = 5.\n"

func TestRunClosesChild(t *testing.T) {
	for _, name := range []string{"success", "output failure", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			pidFile := filepath.Join(t.TempDir(), "pid")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			server := mcp.Server{
				Command: executable,
				Args:    []string{"-test.run=^TestServerProcess$"},
				Env:     map[string]string{"KEYLESS_TEST_PID_FILE": pidFile},
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			writeErr := errors.New("output closed")
			switch name {
			case "output failure":
				writer = writerFunc(func([]byte) (int, error) { return 0, writeErr })
			case "cancelled":
				// The connected status proves that the child started. Cancel
				// before the runner sends its first model request.
				writer = writerFunc(func(p []byte) (int, error) { cancel(); return len(p), nil })
			}
			err = run(ctx, writer, server)
			switch name {
			case "success":
				if err != nil || output.String() != transcript {
					t.Fatalf("err = %v, output = %q", err, output.String())
				}
			case "output failure":
				if !errors.Is(err, writeErr) {
					t.Fatalf("err = %v, want output error", err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want cancellation", err)
				}
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child %d still exists after run: %v", pid, err)
			}
		})
	}
}

func TestRunReportsServerFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := run(ctx, io.Discard, mcp.Server{Command: filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), `MCP server "demo": failed:`) {
		t.Fatalf("err = %v, want failed server status", err)
	}
}

func Example() {
	dir, err := os.MkdirTemp("", "mcp-keyless-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, buildExample(ctx, dir))
	command.Dir = dir // No project config or provider setup exists here.
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("run: %v\n%s", err, output))
	}
	fmt.Print(string(output))
	// Output:
	// MCP server demo: connected (1 tool)
	// MCP result: {"sum":5}
	// Scripted answer: 2 + 3 = 5.
}

func buildExample(ctx context.Context, dir string) string {
	binary := filepath.Join(dir, "keyless")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, output))
	}
	return binary
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
