package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/wonton/cli"
)

// runCLI runs agent-archive with args as the command line would, without
// color, and returns its exit code and output.
func runCLI(t *testing.T, args ...string) *cli.TestResult {
	t.Helper()
	return newApp().Test(t, cli.TestArgs(args...))
}

func TestCommandLine(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	r := runCLI(t, "copy", "--archive", e.opts.ArchiveDir,
		"--claude-dir", e.opts.ClaudeDir, "--codex-dir", e.opts.CodexDir)
	if r.ExitCode != 0 {
		t.Fatalf("copy exit %d: %v %s", r.ExitCode, r.Err, r.Stderr)
	}
	if !r.Contains("originals copied: 16") {
		t.Fatalf("copy output:\n%s", r.Stdout)
	}

	r = runCLI(t, "status", "--archive", e.opts.ArchiveDir)
	if r.ExitCode != 0 {
		t.Fatalf("status exit %d: %v", r.ExitCode, r.Err)
	}
	if !strings.HasSuffix(r.Stdout, "\nneeds a look: 1 warning, no failed files\n") { // the copy run's warning
		t.Errorf("status output lacks its last line:\n%s", r.Stdout)
	}
	for _, want := range []string{"last run:", "originals seen: 16, copied: 16", "failed files: 0",
		"app          kind             current  superseded  gone  failed\n",
		"\nclaude_code  session          1        0           0     0\n",
	} {
		if !r.Contains(want) {
			t.Errorf("status output lacks %q:\n%s", want, r.Stdout)
		}
	}
	if strings.Contains(r.Stdout, "\x1b[") {
		t.Errorf("status output has escape codes without a terminal:\n%q", r.Stdout)
	}

	if r = runCLI(t, "rebuild", "--archive", e.opts.ArchiveDir); r.ExitCode != 0 {
		t.Fatalf("rebuild exit %d: %v", r.ExitCode, r.Err)
	}

	for _, args := range [][]string{
		{"nope"},
		{"copy", "--bogus"},
		{"copy", "-archive", e.opts.ArchiveDir}, // the old single-dash form
		{"status", "extra"},
		{"report", "--by", "nope"},
		{"report", "--since", "yesterday", "--archive", e.opts.ArchiveDir},
	} {
		if r := runCLI(t, args...); r.ExitCode != 1 {
			t.Errorf("%v: exit %d, want 1", args, r.ExitCode)
		}
	}
	if r := runCLI(t, "--help"); r.ExitCode != 0 || !r.Contains("schedule") {
		t.Errorf("help: exit %d:\n%s", r.ExitCode, r.Stdout)
	}
}

// The directories come from environment variables when no flag is given,
// and a flag wins over its variable.
func TestDirectoriesFromEnvironment(t *testing.T) {
	e := newEnv(t)
	writeEverything(e)
	t.Setenv("AGENT_ARCHIVE_DIR", e.opts.ArchiveDir)
	t.Setenv("AGENT_ARCHIVE_CLAUDE_DIR", e.opts.ClaudeDir)
	t.Setenv("AGENT_ARCHIVE_CODEX_DIR", e.opts.CodexDir)
	if r := runCLI(t, "copy"); r.ExitCode != 0 || !r.Contains("originals copied: 16") {
		t.Fatalf("copy exit %d: %v\n%s", r.ExitCode, r.Err, r.Stdout)
	}
	if r := runCLI(t, "status"); !r.Contains("archive: " + e.opts.ArchiveDir) {
		t.Fatalf("status:\n%s", r.Stdout)
	}
	other := t.TempDir()
	if r := runCLI(t, "status", "--archive", other); r.ExitCode != 1 || !strings.Contains(r.Err.Error(), other) {
		t.Fatalf("flag did not win: exit %d: %v", r.ExitCode, r.Err)
	}
	// An empty variable means the default, ~/agent-archive.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_ARCHIVE_DIR", "")
	want := filepath.Join(home, "agent-archive")
	if r := runCLI(t, "status"); r.ExitCode != 1 || !strings.Contains(r.Err.Error(), want) {
		t.Fatalf("empty variable: exit %d: %v", r.ExitCode, r.Err)
	}
}
