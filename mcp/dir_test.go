package mcp

import (
	"context"
	"fmt"
	"github.com/deepnoodle-ai/dashi/tool"
	"os"
	"path/filepath"
)

// ExampleOptions_Dir uses a real self-hosted stdio process without credentials.
func ExampleOptions_Dir() {
	dir, err := os.MkdirTemp("", "mcp-dir-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		panic(err)
	}
	ts, err := Open(context.Background(), Config{Servers: map[string]Server{"local": {Command: serverBinary, Env: map[string]string{"TESTSERVER_CWD_TOOL": "1"}}}}, Options{Dir: dir})
	if err != nil {
		panic(err)
	}
	defer ts.Close()
	tools, err := ts.Tools(context.Background())
	if err != nil {
		panic(err)
	}
	for _, entry := range tools {
		if entry.Definition.Name == "mcp__local__cwd" {
			out, err := entry.Executor.Execute(context.Background(), tool.Call{ID: "cwd", Name: entry.Definition.Name, Args: []byte(`{}`)})
			if err != nil {
				panic(err)
			}
			fmt.Println("Server uses selected directory:", out.Output.Content[0].Text.Text == dir)
		}
	}
	// Output: Server uses selected directory: true
}
