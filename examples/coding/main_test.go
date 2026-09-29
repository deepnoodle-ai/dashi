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
