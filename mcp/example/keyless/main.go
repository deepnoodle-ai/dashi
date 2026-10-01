// Command keyless starts its own stdio MCP server, calls its add tool through
// a Dashi agent, and closes the server. It needs no API key or .mcp.json:
// dashitest supplies fixed model replies, but the MCP connection is real.
//
// From the mcp directory:
//
//	go run ./example/keyless
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/mcp"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// The child uses stdout only for the MCP protocol. Diagnostics use stderr.
	if len(os.Args) == 2 && os.Args[1] == "--serve" {
		if err := serve(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout, mcp.Server{
		Command: executable, Args: []string{"--serve"},
	}); err != nil {
		log.Fatal(err)
	}
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type sumResult struct {
	Sum int `json:"sum"`
}

func serve(ctx context.Context) error {
	server := sdk.NewServer(&sdk.Implementation{Name: "keyless-example", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "add", Description: "Add two integers."},
		func(_ context.Context, _ *sdk.CallToolRequest, args addArgs) (*sdk.CallToolResult, sumResult, error) {
			return nil, sumResult{Sum: args.A + args.B}, nil
		})
	return server.Run(ctx, &sdk.StdioTransport{})
}

func run(ctx context.Context, output io.Writer, server mcp.Server) (err error) {
	// This trusted entry points to this example's executable. No project
	// config is read and no external server needs approval.
	ts, err := mcp.Open(ctx, mcp.Config{
		Servers: map[string]mcp.Server{"demo": server},
	}, mcp.Options{})
	if err != nil {
		return fmt.Errorf("open MCP server: %w", err)
	}
	// Close also reaps the child on a failed run or a cancelled context.
	defer func() { err = errors.Join(err, ts.Close()) }()
	for _, status := range ts.Status() {
		if status.State != mcp.StateConnected {
			return fmt.Errorf("MCP server %q: %s: %v", status.Name, status.State, status.Err)
		}
		if _, err := fmt.Fprintf(output, "MCP server %s: connected (%d tool)\n", status.Name, status.Tools); err != nil {
			return err
		}
	}
	client := dashitest.NewClient(
		dashitest.CallTool("add-1", "mcp__demo__add", `{"a":2,"b":3}`),
		dashitest.Reply("2 + 3 = 5."),
	)
	out, err := (dashi.Runner{Client: client, Tools: tool.Join(ts)}).Run(
		ctx, dashi.NewTurn("mcp-keyless"), dashi.Start{
			Agent: dashi.Agent{Model: dashitest.Model},
			Input: []llm.Message{llm.UserMessage(llm.Text("Add 2 and 3."))},
		})
	if err != nil {
		return fmt.Errorf("run agent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if out.Stop.Kind != dashi.StopCompleted {
		return fmt.Errorf("agent stopped: %s", out.Stop.Kind)
	}
	// Show the actual server result seen by the model. The final reply is
	// scripted, so it alone cannot prove that the MCP call worked.
	for _, input := range client.Inputs() {
		for _, message := range input.Messages {
			for _, part := range message.Content {
				result := part.ToolResult
				if result == nil {
					continue
				}
				if result.IsError {
					return fmt.Errorf("MCP add tool failed: %v", result.Output)
				}
				for _, block := range result.Output.Content {
					if block.Text != nil {
						if _, err := fmt.Fprintf(output, "MCP result: %s\n", block.Text.Text); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	_, err = fmt.Fprintf(output, "Scripted answer: %s\n", out.Reply)
	return err
}
