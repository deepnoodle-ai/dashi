package workspace

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// Repeated ** segments match like one, without backtracking through every
// way to split the path among them.
func TestMatchGlobCollapsesRepeatedDoubleStars(t *testing.T) {
	pattern := strings.Repeat("**/", 40) + "x"
	name := strings.Repeat("d/", 40) + "y"
	done := make(chan bool)
	go func() { done <- matchGlob(pattern, name) }()
	select {
	case got := <-done:
		if got {
			t.Fatal("matched a name that does not end in x")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("matchGlob did not finish")
	}
	if !matchGlob(pattern, "d/e/x") || !matchGlob("a/**/**/b", "a/b") {
		t.Fatal("collapsed pattern lost a match")
	}
}

func TestMatchGlobDoesNotBacktrackOnSeparateDoubleStars(t *testing.T) {
	pattern := strings.Repeat("**/a/", 7) + "x"
	name := strings.TrimSuffix(strings.Repeat("a/", 60), "/")
	done := make(chan bool)
	go func() { done <- matchGlob(pattern, name) }()
	select {
	case got := <-done:
		if got {
			t.Fatal("matched a name that does not end in x")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("matchGlob did not finish")
	}
	if !matchGlob(pattern, name+"/x") || !matchGlob("**/a/**/b", "a/c/b") || matchGlob("**/a/**/b", "c/b") {
		t.Fatal("memoized matching changed a result")
	}
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
