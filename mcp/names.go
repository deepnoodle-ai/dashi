package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxNameLen is the longest tool name that every model provider accepts.
const maxNameLen = 64

// toolName returns the model-facing name of a remote tool. It depends only on
// the server name and the remote name, never on start order, so a resumed
// session finds the same tool. The form matches Claude Code, so a user keeps
// the permission rules written for it.
func toolName(server, remote string) string {
	name := "mcp__" + sanitize(server) + "__" + sanitize(remote)
	if len(name) <= maxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:maxNameLen-7] + "_" + hex.EncodeToString(sum[:3])
}

// sanitize replaces each character that a provider may reject with "_".
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}
