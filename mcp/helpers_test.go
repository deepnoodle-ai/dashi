package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/deepnoodle-ai/dashi/mcp/internal/testserver"
)

// serverBinary is the compiled test server, built once for all tests.
var serverBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-testserver")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	serverBinary = filepath.Join(dir, "testserver")
	build := exec.Command(
		"go", "build", "-o", serverBinary, "./internal/testserver/cmd/testserver",
	)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build test server: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// stdioServer returns an entry that runs the test server and logs to a file
// in the test's temp directory. It returns the log path.
func stdioServer(t *testing.T) (Server, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "server.log")
	return Server{
		Command: serverBinary,
		Env:     map[string]string{"TESTSERVER_LOG": log},
	}, log
}

// httpServer runs the test server over streamable HTTP. It returns the entry
// and the log path.
func httpServer(t *testing.T, h func(http.Handler) http.Handler) (Server, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "server.log")
	srv := testserver.New(testserver.Options{Log: log})
	var handler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		nil,
	)
	if h != nil {
		handler = h(handler)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return Server{Type: TypeHTTP, URL: ts.URL}, log
}

// open opens cfg and closes the toolset when the test ends.
func open(t *testing.T, servers map[string]Server, opts Options) *Toolset {
	t.Helper()
	ts, err := Open(t.Context(), Config{Servers: servers}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	return ts
}

func statusOf(t *testing.T, ts *Toolset, name string) ServerStatus {
	t.Helper()
	for _, s := range ts.Status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for server %q", name)
	return ServerStatus{}
}

func find(t *testing.T, ts *Toolset, name string) tool.Tool {
	t.Helper()
	tools, err := ts.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Definition.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q is not offered", name)
	return tool.Tool{}
}

func toolNames(t *testing.T, ts *Toolset) []string {
	t.Helper()
	tools, _ := ts.Tools(t.Context())
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Definition.Name)
	}
	return names
}

func run(
	t *testing.T,
	ts *Toolset,
	name, args string,
) (tool.Outcome, error) {
	t.Helper()
	return runCtx(t.Context(), t, ts, name, args)
}

func runCtx(
	ctx context.Context,
	t *testing.T,
	ts *Toolset,
	name, args string,
) (tool.Outcome, error) {
	t.Helper()
	tl := find(t, ts, name)
	return tl.Executor.Execute(ctx, tool.Call{
		ID:   "c1",
		Name: name,
		Args: json.RawMessage(args),
	})
}

// texts returns the text of each text block of an outcome.
func texts(o tool.Outcome) []string {
	var out []string
	for _, b := range o.Output.Content {
		if b.Text != nil {
			out = append(out, b.Text.Text)
		}
	}
	return out
}

func readLog(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func contains(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}
