package main

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/workspace"
)

func TestDescribeShowsWholeCommand(t *testing.T) {
	r := workspace.ApprovalRequest{
		Tool:    workspace.ToolBash,
		Summary: "run: echo ok...",
		Dir:     "/work",
		Command: "echo ok\nrm -rf build",
	}
	got := describe(r)
	for _, want := range []string{"/work", "echo ok\nrm -rf build"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt %q lacks %q", got, want)
		}
	}
}

func TestDescribeEscapesControls(t *testing.T) {
	for _, r := range []workspace.ApprovalRequest{
		{
			Tool:    workspace.ToolBash,
			Dir:     "/work\x1b[31m",
			Command: "echo ok\x1b[2K\rrm -rf build",
		},
		{
			Tool:    workspace.ToolWriteFile,
			Summary: "write a\x1b[2J",
			Preview: "+line\rfake",
		},
	} {
		got := describe(r)
		if strings.ContainsAny(got, "\x1b\r") {
			t.Fatalf("prompt %q holds a raw control character", got)
		}
		if !strings.Contains(got, "\u241b") || (r.Command != "" && !strings.Contains(got, "\u240d")) {
			t.Fatalf("prompt %q does not show the controls as text", got)
		}
	}
}
