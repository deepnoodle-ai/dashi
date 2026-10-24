package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
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
		{"interrupt", Block{Kind: KindStop, Text: "Interrupted.", State: string(dashi.StopInterrupted)}, []string{"38;5;248m■ Interrupted."}, []string{"33m"}},
		{"limit", Block{Kind: KindStop, Text: "Stopped after 3.", State: string(dashi.StopLimited)}, []string{"33m■ Stopped after 3."}, nil},
		{"canceled", Block{Kind: KindStop, Text: "Canceled.", State: string(dashi.StopCanceled)}, []string{"38;5;248m■ Canceled."}, []string{"33m"}},
		{"failed", Block{Kind: KindStop, Text: "Failed.", State: string(dashi.StopFailed)}, []string{"31m■ Failed."}, []string{"33m"}},
		{"refused", Block{Kind: KindStop, Text: "Refused.", State: string(dashi.StopRefused)}, []string{"31m■ Refused."}, []string{"33m"}},
		{"unknown", Block{Kind: KindStop, Text: "Unknown.", State: string(dashi.StopResultUnknown)}, []string{"31m■ Unknown."}, []string{"33m"}},
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

// useTheme rebuilds every role and the styles built from them, and
// markdown picks the matching syntax style.
func TestUseThemeRebuildsStyles(t *testing.T) {
	t.Cleanup(func() { useTheme(false) })
	render := func() string {
		return tui.Sprint(tui.Stack(
			tui.Text("m").Style(mutedStyle),
			tui.Text("h").Style(hintStyle),
			tui.Text("f").Style(faintStyle),
			tui.Text("a").Style(commandHighlightStyle),
			tui.Text("c").Style(codeStyle),
			tui.Text("p").Style(promptStyle),
			tui.Text("q").Style(mutedStyle.WithBackground(promptBackground)),
		), tui.WithWidth(20), tui.WithColor(true))
	}
	dark := render()
	useTheme(true)
	light := render()
	for _, want := range []string{"38;5;242m", "38;5;250m", "38;5;26m", "48;5;255m", "38;5;235;48;5;254m", "38;5;242;48;5;254m"} {
		if !strings.Contains(light, want) {
			t.Errorf("light: missing %q in %q", want, light)
		}
	}
	for _, old := range []string{"38;5;248", "38;5;240", "38;5;75", "48;5;236", "38;5;252"} {
		if strings.Contains(light, old) {
			t.Errorf("light: kept dark %q in %q", old, light)
		}
	}
	if got := markdownTheme().SyntaxTheme; got != "dashi-light" {
		t.Errorf("light syntax theme = %q", got)
	}
	useTheme(false)
	if got := render(); got != dark {
		t.Errorf("dark again = %q, want %q", got, dark)
	}
	if got := markdownTheme().SyntaxTheme; got != "dashi" {
		t.Errorf("dark syntax theme = %q", got)
	}
}

// stubBackground replaces the terminal query for one test and counts calls.
func stubBackground(t *testing.T, background tui.TerminalBackground, err error) *int {
	t.Helper()
	calls := 0
	old := detectBackground
	detectBackground = func(time.Duration) (tui.TerminalBackground, error) {
		calls++
		return background, err
	}
	t.Cleanup(func() { detectBackground = old })
	return &calls
}

// Auto follows the terminal's answer, and unknown or a failed query is dark.
// Dark and light never ask, and neither does auto without color.
func TestLightTheme(t *testing.T) {
	for _, tc := range []struct {
		setting    string
		color      bool
		background tui.TerminalBackground
		err        error
		want       bool
		asks       bool
	}{
		{"", true, tui.TerminalBackgroundLight, nil, true, true},
		{"auto", true, tui.TerminalBackgroundLight, nil, true, true},
		{"auto", true, tui.TerminalBackgroundDark, nil, false, true},
		{"auto", true, tui.TerminalBackgroundUnknown, nil, false, true},
		{"auto", true, tui.TerminalBackgroundLight, errors.New("write failed"), true, true},
		{"auto", true, tui.TerminalBackgroundUnknown, errors.New("write failed"), false, true},
		{"auto", false, tui.TerminalBackgroundLight, nil, false, false},
		{"light", true, tui.TerminalBackgroundDark, nil, true, false},
		{"light", false, tui.TerminalBackgroundDark, nil, true, false},
		{"dark", true, tui.TerminalBackgroundLight, nil, false, false},
	} {
		calls := stubBackground(t, tc.background, tc.err)
		if got := lightTheme(tc.setting, tc.color); got != tc.want || (*calls > 0) != tc.asks {
			t.Errorf("lightTheme(%q, %v) with %v = %v after %d queries; want %v, asks %v", tc.setting, tc.color, tc.background, got, *calls, tc.want, tc.asks)
		}
	}
}

// Line mode and redirected input draw no screen, so they never ask the
// terminal about its background.
func TestNoScreenSkipsBackgroundQuery(t *testing.T) {
	calls := stubBackground(t, tui.TerminalBackgroundLight, nil)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	// Empty input sends no request, so no case calls a model.
	for _, args := range [][]string{{"--plain", "--theme", "auto"}, {"--theme", "auto"}, {"run", "--continue"}} {
		var stdout, stderr bytes.Buffer
		run(t.Context(), args, strings.NewReader(""), &stdout, &stderr)
	}
	if *calls != 0 {
		t.Fatalf("asked the terminal %d times", *calls)
	}
}
