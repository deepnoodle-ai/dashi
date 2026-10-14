package mcp

import (
	"strings"
	"testing"
)

func TestToolNameUsesServerAndToolName(t *testing.T) {
	tests := []struct {
		server, remote, want string
	}{
		{"github", "create_issue", "mcp__github__create_issue"},
		{"my server", "a.b", "mcp__my_server__a_b"},
		{"docs", "Fetch-Page", "mcp__docs__Fetch-Page"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := toolName(tt.server, tt.remote); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolNameCutsLongNamesWithStableHash(t *testing.T) {
	long := strings.Repeat("x", 80)
	a := toolName("s", long)
	b := toolName("s", long)
	c := toolName("s", long+"y")
	if len(a) != maxNameLen {
		t.Fatalf("len = %d, want %d", len(a), maxNameLen)
	}
	if a != b {
		t.Fatal("name changed between calls")
	}
	if a == c {
		t.Fatal("different long names share one short name")
	}
}

func TestToolNameHashesTheRawNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	a := toolName("s", long+".a")
	b := toolName("s", long+"/a")
	if a == b {
		t.Fatalf("names that differ before sanitizing share %q", a)
	}
}
