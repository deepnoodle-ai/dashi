package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/tui"
)

// TestColorsGoThroughTheme keeps every color in theme.go, so the terminal
// UI has one palette. withoutColors may reset colors with tui.ColorDefault.
func TestColorsGoThroughTheme(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file == "theme.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ReplaceAll(line, "tui.ColorDefault", ""), "tui.Color") {
				t.Errorf("%s:%d uses a color outside theme.go: %s", file, n+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestStatusColorsFollowRoles checks the lines whose color says whether the
// user must act: only a limit stop is a warning, a denial is settled, and
// the user's queued words keep the normal color.
func TestStatusColorsFollowRoles(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    Block
		want []string
		not  []string
	}{
		{"interrupt", Block{Kind: KindStop, Text: "Interrupted.", State: "interrupted"}, []string{"38;5;248m■ Interrupted."}, []string{"33m"}},
		{"limit", Block{Kind: KindStop, Text: "Stopped after 3.", State: "limited"}, []string{"33m■ Stopped after 3."}, nil},
		{"denied", Block{Kind: KindTool, Name: "bash", Arg: "rm -rf build", State: StateDenied}, []string{"38;5;248m⊘", "38;5;248mdenied"}, []string{"✗", "33m"}},
		{"steer", Block{Kind: KindSteer, Text: "also check"}, []string{"38;5;75m↳", "38;5;248m steer queued: \x1b[0malso check"}, []string{"33m"}},
	} {
		got := tui.Sprint(blockView(tc.b), tui.WithWidth(80), tui.WithColor(true))
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in %q", tc.name, w, got)
			}
		}
		for _, w := range tc.not {
			if strings.Contains(got, w) {
				t.Errorf("%s: unexpected %q in %q", tc.name, w, got)
			}
		}
	}
}
