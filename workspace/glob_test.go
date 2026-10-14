package workspace

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/tool"
)

func TestExpandBraces(t *testing.T) {
	tests := []struct {
		pattern string
		want    []string
	}{
		{"*.go", []string{"*.go"}},
		{"*.{ts,tsx}", []string{"*.ts", "*.tsx"}},
		{"{a,b}/{c,d}", []string{"a/c", "a/d", "b/c", "b/d"}},
		{"*.{go,{ts,tsx}}", []string{"*.go", "*.ts", "*.tsx"}},
		{"x{,y}", []string{"x", "xy"}},
		{"a{b", []string{"a{b"}},
		{"a{b{c,d}", []string{"a{bc", "a{bd"}},
	}
	for _, tc := range tests {
		got, err := expandBraces(tc.pattern)
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func TestExpandBracesCapsPatterns(t *testing.T) {
	// Six groups make exactly 64 patterns, the most allowed.
	if got, err := expandBraces(strings.Repeat("{a,b}", 6)); err != nil || len(got) != maxBracePatterns {
		t.Fatalf("six groups: %d patterns, err %v", len(got), err)
	}
	// Thirty groups would make 2^30 patterns. The expansion must stop early.
	_, err := expandBraces(strings.Repeat("{a,b}", 30))
	if err == nil || !strings.Contains(err.Error(), "more than 64 patterns") {
		t.Fatalf("err = %v", err)
	}
}

func TestGlobRefusesHugeBraceExpansion(t *testing.T) {
	list, _ := newTestTools(t)
	out, text := call(t, list, ToolGlob, map[string]any{"pattern": strings.Repeat("{a,b}", 30)})
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "fewer {a,b} groups")
}

func TestGlobStopsWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.go", "")
	s, err := newScope(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _ := glob(ctx, s, globArgs{Pattern: "*.go"})
	text := out.Output.Content[0].Text.Text
	wantState(t, out, text, tool.StateFailed)
	wantContains(t, text, "canceled")
}
