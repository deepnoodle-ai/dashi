// Command testserver runs the test MCP server over stdio. Set
// TESTSERVER_LOG to a file path to record events.
package main

import (
	"context"
	"os"

	"github.com/deepnoodle-ai/dashi/mcp/internal/testserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := testserver.New(testserver.Options{Log: os.Getenv("TESTSERVER_LOG")})
	// Run ends with an error when the client closes stdin; that is a normal
	// shutdown for this server.
	server.Run(context.Background(), &mcp.StdioTransport{})
}
