// Package testserver is a tiny MCP server for the mcp package tests and
// example. It uses the same SDK as the client. It is not part of the public
// API.
//
// Tools: echo, add, fail, image, structured, blob, slow, pid, exit, and
// addtool. Each call appends one line to Log when it is set, so a test can
// see what the server did, or that it never ran.
package testserver

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pngPixel is a valid one-pixel PNG.
var pngPixel = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xff, 0xff, 0x3f,
	0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59, 0xe7, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// Options configures a test server.
type Options struct {
	// Log is a file that receives one line per event. Empty disables it.
	Log string
}

// TextArgs is the input of echo.
type TextArgs struct {
	Text string `json:"text"`
}

// AddArgs is the input of add.
type AddArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

// SumResult is the structured output of add.
type SumResult struct {
	Sum int `json:"sum"`
}

// New returns a server with the standard test tools.
func New(opts Options) *mcp.Server {
	s := &state{log: opts.Log}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s.logf("start")
	if os.Getenv("TESTSERVER_CWD_TOOL") != "" {
		s.server.AddTool(&mcp.Tool{Name: "cwd", InputSchema: emptySchema()}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			dir, err := os.Getwd()
			return text(dir), err
		})
	}

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "echo", Description: "Return the text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	},
		func(_ context.Context, _ *mcp.CallToolRequest, in TextArgs) (*mcp.CallToolResult, any, error) {
			s.logf("call echo")
			return text(in.Text), nil, nil
		})
	mcp.AddTool(s.server, &mcp.Tool{Name: "add", Description: "Add two numbers."},
		func(_ context.Context, _ *mcp.CallToolRequest, in AddArgs) (*mcp.CallToolResult, SumResult, error) {
			s.logf("call add")
			return nil, SumResult{Sum: in.A + in.B}, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "fail", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call fail")
			res := text("it broke")
			res.IsError = true
			return res, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "image", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call image")
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "a pixel"},
				&mcp.ImageContent{Data: pngPixel, MIMEType: "image/png"},
			}}, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "structured", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call structured")
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "done"}},
				StructuredContent: map[string]any{"ok": true},
			}, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "blob", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call blob")
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI:      "file:///data.bin",
					MIMEType: "application/octet-stream",
					Blob:     []byte{1, 2, 3, 4},
				}},
				&mcp.ResourceLink{Name: "notes", URI: "file:///notes.txt"},
			}}, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "slow", InputSchema: emptySchema()},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call slow")
			select {
			case <-ctx.Done():
				s.logf("cancelled slow")
				return nil, ctx.Err()
			case <-time.After(time.Minute):
				return text("late"), nil
			}
		})
	s.server.AddTool(
		&mcp.Tool{Name: "pid", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text(fmt.Sprint(os.Getpid())), nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "exit", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call exit")
			os.Exit(3)
			return nil, nil
		})
	s.server.AddTool(
		&mcp.Tool{Name: "addtool", InputSchema: emptySchema()},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.logf("call addtool")
			s.server.AddTool(
				&mcp.Tool{Name: "late", InputSchema: emptySchema()},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return text("late tool"), nil
				})
			return text("added"), nil
		})
	return s.server
}

type state struct {
	server *mcp.Server
	log    string
	mu     sync.Mutex
}

func (s *state) logf(format string, args ...any) {
	if s.log == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object"}
}
