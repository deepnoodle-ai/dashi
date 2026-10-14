package mcp

import (
	"context"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// groupTransport runs a stdio server in its own process group and stops the
// whole group when the connection closes. A server started through a wrapper,
// such as npx or a shell script, may leave children that the SDK's shutdown
// of the direct child does not reach.
type groupTransport struct {
	mcp.CommandTransport
}

func (g *groupTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := g.CommandTransport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &groupConn{Connection: c, cmd: g.Command}, nil
}

// groupConn stops the child's process group after the SDK's shutdown of the
// child finishes.
type groupConn struct {
	mcp.Connection
	cmd *exec.Cmd
}

func (c *groupConn) Close() error {
	err := c.Connection.Close()
	stopGroup(c.cmd)
	return err
}
