package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
)

// This keyless CLI example starts a real documentation server, reads its
// relative guide through MCP, and closes it. Only model replies are scripted.
func Example_mcpDocumentation() {
	dir, err := os.MkdirTemp("", "dashi-mcp-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	binary, err := os.Executable()
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"docs": map[string]any{"command": binary, "args": []string{"-test.run=^TestMCPServerProcess$", "--", "guide.md"}, "env": map[string]string{"DASHI_MCP_TEST_PROCESS": "1", "DASHI_MCP_TEST_LOG": filepath.Join(dir, "process.log")}}}})
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "tools.json"), raw, 0600); err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "guide.md"), []byte("Recorded facts preserve recovery."), 0600); err != nil {
		panic(err)
	}
	client := &fakeClient{script: []reply{callTool("read-guide", "mcp__docs__fetch", `{"path":"guide.md"}`), say("Guide fetched.")}}
	cfg := appConfig{Dir: dir, HomeDir: dir, InvocationDir: dir, MCPFiles: []string{"tools.json"}, Model: "fixture", Client: client, Store: session.NewStore(filepath.Join(dir, "sessions"))}
	var stdout, stderr bytes.Buffer
	code := runPrint(context.Background(), cfg, Options{Print: "Read the recovery guide.", PrintSet: true, Yes: true}, &stdout, &stderr)
	if code != 0 {
		panic(stderr.String())
	}
	fmt.Println("CLI exit:", code)
	fmt.Println("Actual guide reached the model:", strings.Contains(inputText(client.inputAt(1)), "Recorded facts preserve recovery."))
	fmt.Print(stdout.String())
	// Output:
	// CLI exit: 0
	// Actual guide reached the model: true
	// Guide fetched.
}
